package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"github.com/prairie-server/prairie-plugin-sdk/pkg/pluginsdk/runtime"
)

// The tests in this file swap package-level seams, so none of them runs in
// parallel. Go releases the parallel tests only after every sequential
// top-level test has finished, so the swaps never race them.

// restoreSeams puts every seam and the build version back after a test.
func restoreSeams(t *testing.T) {
	t.Helper()

	originalManifest := manifestJSON
	originalExecutable := osExecutable
	originalReadFile := osReadFile
	originalServe := runtimeServe
	originalVersion := version
	t.Cleanup(func() {
		manifestJSON = originalManifest
		osExecutable = originalExecutable
		osReadFile = originalReadFile
		runtimeServe = originalServe
		version = originalVersion
	})
}

func TestMainWiresRuntimeServers(t *testing.T) {
	restoreSeams(t)

	binary := []byte("fake plugin binary")
	osExecutable = func() (string, error) { return "/plugins/mdblist", nil }
	osReadFile = func(path string) ([]byte, error) {
		if path != "/plugins/mdblist" {
			t.Errorf("read %q, want the resolved executable path", path)
		}
		return binary, nil
	}

	var served *runtime.ServeConfig
	runtimeServe = func(cfg runtime.ServeConfig) { served = &cfg }

	main()

	if served == nil {
		t.Fatal("main() did not call runtime.Serve")
	}
	rs, ok := served.Servers.Runtime.(*runtimeServer)
	if !ok {
		t.Fatalf("Runtime server = %T, want *runtimeServer", served.Servers.Runtime)
	}
	ms, ok := served.Servers.MetadataProvider.(*metadataServer)
	if !ok {
		t.Fatalf("MetadataProvider server = %T, want *metadataServer", served.Servers.MetadataProvider)
	}
	if ms.runtime != rs {
		t.Fatal("metadata server does not share the runtime server's client")
	}
	if rs.client == nil {
		t.Fatal("runtime server has no MDBList client")
	}

	response, err := rs.GetManifest(context.Background(), &pluginv1.GetManifestRequest{})
	if err != nil {
		t.Fatalf("GetManifest() returned error: %v", err)
	}
	sum := sha256.Sum256(binary)
	if got, want := response.GetManifest().GetChecksum(), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("served manifest checksum = %q, want %q", got, want)
	}
	if got, want := response.GetManifest().GetPluginId(), "prairie.mdblist"; got != want {
		t.Fatalf("served manifest plugin_id = %q, want %q", got, want)
	}
}

func TestMainPanicsOnAnUnloadableManifest(t *testing.T) {
	restoreSeams(t)

	manifestJSON = []byte(`{`)
	runtimeServe = func(runtime.ServeConfig) {
		t.Error("runtime.Serve called despite an unloadable manifest")
	}

	defer func() {
		recovered := recover()
		err, ok := recovered.(error)
		if !ok || !strings.Contains(err.Error(), "load embedded manifest") {
			t.Fatalf("main() panicked with %v, want the manifest load error", recovered)
		}
	}()
	main()
}

func TestGetManifestReturnsTheLoadedManifest(t *testing.T) {
	manifest := &pluginv1.PluginManifest{PluginId: "prairie.mdblist", Version: "9.9.9"}
	rs := &runtimeServer{manifest: manifest}

	response, err := rs.GetManifest(context.Background(), &pluginv1.GetManifestRequest{})
	if err != nil {
		t.Fatalf("GetManifest() returned error: %v", err)
	}
	if response.GetManifest() != manifest {
		t.Fatalf("GetManifest() = %v, want the server's manifest", response.GetManifest())
	}
}

func TestLoadManifestErrorPaths(t *testing.T) {
	restoreSeams(t)
	original := manifestJSON

	manifestJSON = []byte(`{`)
	if _, err := loadManifest(); err == nil || !strings.Contains(err.Error(), "load embedded manifest") {
		t.Fatalf("loadManifest() error = %v, want an embedded manifest error", err)
	}
	manifestJSON = original

	errNoExecutable := errors.New("no executable")
	osExecutable = func() (string, error) { return "", errNoExecutable }
	if _, err := loadManifest(); !errors.Is(err, errNoExecutable) {
		t.Fatalf("loadManifest() error = %v, want it to wrap the executable error", err)
	}
	osExecutable = func() (string, error) { return "/plugins/mdblist", nil }

	errRead := errors.New("read failed")
	osReadFile = func(string) ([]byte, error) { return nil, errRead }
	_, err := loadManifest()
	if !errors.Is(err, errRead) || !strings.Contains(err.Error(), `"/plugins/mdblist"`) {
		t.Fatalf("loadManifest() error = %v, want the read error naming the path", err)
	}
}

func TestLoadManifestUsesTheBuildVersion(t *testing.T) {
	restoreSeams(t)
	osExecutable = func() (string, error) { return "/plugins/mdblist", nil }
	osReadFile = func(string) ([]byte, error) { return []byte("x"), nil }

	version = "1.2.3-test"
	manifest, err := loadManifest()
	if err != nil {
		t.Fatalf("loadManifest() returned error: %v", err)
	}
	if got := manifest.GetVersion(); got != "1.2.3-test" {
		t.Fatalf("manifest version = %q, want the -ldflags version", got)
	}

	version = ""
	manifest, err = loadManifest()
	if err != nil {
		t.Fatalf("loadManifest() returned error: %v", err)
	}
	if got := manifest.GetVersion(); got == "" || got == "1.2.3-test" {
		t.Fatalf("manifest version = %q, want manifest.json's own version", got)
	}
}

// userAgentSent makes one lookup through client and returns the User-Agent
// MDBList saw.
func userAgentSent(t *testing.T, build func() string) string {
	t.Helper()

	var mu sync.Mutex
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.Header.Get("User-Agent")
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	version = build()
	client := newClient()
	client.SetBaseURL(server.URL)
	client.SetAPIKey("k")
	client.SetBatchWindow(0)
	if _, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0073195"); err != nil {
		t.Fatalf("FetchMedia() returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	return seen
}

func TestNewClientNamesTheBuildInItsUserAgent(t *testing.T) {
	restoreSeams(t)

	if got, want := userAgentSent(t, func() string { return "2.0.1" }), "prairie-plugin-metadata-mdblist/2.0.1"; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
	if got, want := userAgentSent(t, func() string { return "" }), "prairie-plugin-metadata-mdblist"; got != want {
		t.Fatalf("User-Agent without a build version = %q, want %q", got, want)
	}
}

func TestLookupStatusPassesOtherErrorsThrough(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("something else")} {
		got := lookupStatus(err)
		if got != err {
			t.Fatalf("lookupStatus(%v) = %v, want the error unchanged", err, got)
		}
	}
}

func TestStructFromMapDropsUnencodableValues(t *testing.T) {
	if got := structFromMap(map[string]any{"bad": make(chan int)}); got != nil {
		t.Fatalf("structFromMap(unencodable) = %v, want nil", got)
	}
	if got := structFromMap(map[string]any{}); got != nil {
		t.Fatalf("structFromMap(empty) = %v, want nil", got)
	}
	got := structFromMap(map[string]any{"advisory_age": 13})
	if got.GetFields()["advisory_age"].GetNumberValue() != 13 {
		t.Fatalf("structFromMap() = %v, want advisory_age 13", got)
	}
}
