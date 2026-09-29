package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prairie-server/prairie-plugin-metadata-mdblist/metadata"
)

var imdbMovies = batchKey{idProvider: "imdb", mediaType: "movie"}

// answerEveryID is a fake MDBList that knows every IMDb ID it is asked for,
// on both the batch and the single route. An ID's rating is its number.
func answerEveryID(w http.ResponseWriter, req recordedRequest) {
	answer := func(id string) string {
		number, _ := strconv.Atoi(strings.TrimPrefix(id, "tt"))
		return movieBody(id, number, float64(number))
	}
	if req.method != http.MethodPost {
		_, _ = io.WriteString(w, answer(req.path[strings.LastIndex(req.path, "/")+1:]))
		return
	}
	elements := make([]string, 0, len(req.ids))
	for _, id := range req.ids {
		elements = append(elements, answer(id.(string)))
	}
	_, _ = io.WriteString(w, "["+strings.Join(elements, ",")+"]")
}

func requestShapes(requests []recordedRequest) []string {
	shapes := make([]string, 0, len(requests))
	for _, req := range requests {
		if req.method == http.MethodPost {
			shapes = append(shapes, "POST "+strconv.Itoa(len(req.ids)))
		} else {
			shapes = append(shapes, "GET "+req.path)
		}
	}
	return shapes
}

// TestResolveSplitsAQueueLargerThanTheLearnedLimit covers a queue that filled
// before the route's limit dropped: it goes out in chunks of the new limit.
func TestResolveSplitsAQueueLargerThanTheLearnedLimit(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, answerEveryID)
	client := api.client(0)
	client.lowerBatchLimit(imdbMovies, 2)

	ids := []string{"tt0000001", "tt0000002", "tt0000003", "tt0000004", "tt0000005"}
	results, accepted := client.resolve(context.Background(), imdbMovies, ids)

	if !accepted {
		t.Fatal("resolve() accepted = false, want true when every chunk was answered")
	}
	for i, id := range ids {
		result := results[id]
		if result.err != nil {
			t.Fatalf("%s: error %v", id, result.err)
		}
		if got := imdbRating(result.response); got != float64(i+1) {
			t.Fatalf("%s: rating %v, want its own answer %d", id, got, i+1)
		}
	}
	want := []string{"POST 2", "POST 2", "GET /imdb/movie/tt0000005"}
	if got := requestShapes(api.seen()); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestResolveReportsAChunkThatWasNotAccepted(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		if strings.HasSuffix(req.path, "/tt0000003") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		answerEveryID(w, req)
	})
	client := api.client(0)
	client.lowerBatchLimit(imdbMovies, 2)

	results, accepted := client.resolve(context.Background(), imdbMovies, []string{"tt0000001", "tt0000002", "tt0000003"})

	if accepted {
		t.Fatal("resolve() accepted = true, want false when a chunk failed")
	}
	if !errors.Is(results["tt0000003"].err, ErrUnavailable) {
		t.Fatalf("tt0000003 error = %v, want ErrUnavailable", results["tt0000003"].err)
	}
	if results["tt0000001"].response == nil || results["tt0000002"].response == nil {
		t.Fatalf("the answered chunk lost its titles: %+v", results)
	}
}

func TestBatchKeyRejectionFailsEveryLookupWithoutSplitting(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
			w.WriteHeader(code)
		})
		client := api.client(0)

		ids := []string{"tt0000001", "tt0000002", "tt0000003"}
		results, accepted := client.resolve(context.Background(), imdbMovies, ids)

		if accepted {
			t.Fatalf("HTTP %d: accepted = true, want false", code)
		}
		for _, id := range ids {
			if !errors.Is(results[id].err, ErrKeyRejected) {
				t.Fatalf("HTTP %d: %s error = %v, want ErrKeyRejected", code, id, results[id].err)
			}
		}
		if got := len(api.seen()); got != 1 {
			t.Fatalf("HTTP %d: made %d requests, want the one batch", code, got)
		}
	}
}

// TestUnrecognisedBatchAnswerFallsBackToSingles covers a 200 that is neither
// the documented array nor an error object.
func TestUnrecognisedBatchAnswerFallsBackToSingles(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		if req.method == http.MethodPost {
			_, _ = io.WriteString(w, `{"items":"not what the schema says"}`)
			return
		}
		answerEveryID(w, req)
	})
	client := api.client(0)

	results, accepted := client.resolve(context.Background(), imdbMovies, []string{"tt0000001", "tt0000002"})

	if accepted {
		t.Fatal("accepted = true, want false for a refused batch")
	}
	if imdbRating(results["tt0000001"].response) != 1 || imdbRating(results["tt0000002"].response) != 2 {
		t.Fatalf("singles did not answer each ID: %+v", results)
	}
	want := []string{"POST 2", "GET /imdb/movie/tt0000001", "GET /imdb/movie/tt0000002"}
	if got := requestShapes(api.seen()); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	client.batchMu.Lock()
	limit := client.batchLimitLocked(imdbMovies)
	client.batchMu.Unlock()
	if limit != 1 {
		t.Fatalf("batch limit = %d, want 1 once both singles went through", limit)
	}
}

// TestBatchDropsElementsItCannotAttribute pins which batch elements count as
// answers: undecodable, error and "response": false elements are skipped, as
// is a title nobody asked for.
func TestBatchDropsElementsItCannotAttribute(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		_, _ = io.WriteString(w, `[
			5,
			{"error":"not found","ids":{"imdb":"tt0000002"}},
			{"response":false,"ids":{"imdb":"tt0000003"}},
			`+movieBody("tt9999999", 9, 9)+`,
			`+movieBody("TT0000001", 1, 7.5)+`
		]`)
	})
	client := api.client(0)

	answers, outcome, err := client.fetchBatch(context.Background(), imdbMovies, []string{"tt0000001", "tt0000002", "tt0000003"})

	if err != nil || outcome != batchAnswered {
		t.Fatalf("fetchBatch() = outcome %v, err %v, want an answered batch", outcome, err)
	}
	if len(answers) != 1 {
		t.Fatalf("answers = %v, want only tt0000001", answers)
	}
	if got := imdbRating(answers["tt0000001"]); got != 7.5 {
		t.Fatalf("tt0000001 rating = %v, want 7.5 from its upper-case-ID element", got)
	}
}

func TestAnsweredIDPerProvider(t *testing.T) {
	t.Parallel()

	response := &mediaResponse{IDs: mediaIDs{IMDB: " TT0073195 ", TMDB: "578"}}
	tests := map[string]string{
		"imdb":  "tt0073195",
		"tmdb":  "578",
		"trakt": "",
	}
	for idProvider, want := range tests {
		if got := answeredID(batchKey{idProvider: idProvider, mediaType: "movie"}, response); got != want {
			t.Fatalf("answeredID(%s) = %q, want %q", idProvider, got, want)
		}
	}
}

func TestLowerBatchLimitNeverDropsBelowOneOrRises(t *testing.T) {
	t.Parallel()

	client := NewClient()
	limit := func() int {
		client.batchMu.Lock()
		defer client.batchMu.Unlock()
		return client.batchLimitLocked(imdbMovies)
	}

	client.lowerBatchLimit(imdbMovies, 0)
	if got := limit(); got != 1 {
		t.Fatalf("limit after lowering to 0 = %d, want 1", got)
	}
	client.lowerBatchLimit(imdbMovies, 50)
	if got := limit(); got != 1 {
		t.Fatalf("limit after a larger value = %d, want it to stay 1", got)
	}
}

// TestFlushIgnoresAQueueThatAlreadyWent covers the window timer firing after
// the queue was sent full: it must not send it again.
func TestFlushIgnoresAQueueThatAlreadyWent(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, answerEveryID)
	client := api.client(time.Hour)

	answer := make(chan lookupResult, 1)
	stale := &batchQueue{
		ids:     []string{"tt0000001"},
		waiters: map[string][]chan lookupResult{"tt0000001": {answer}},
	}
	current := &batchQueue{waiters: make(map[string][]chan lookupResult)}

	client.flush(imdbMovies, stale) // no queue for the route at all
	client.batchMu.Lock()
	client.queues[imdbMovies] = current
	client.batchMu.Unlock()
	client.flush(imdbMovies, stale) // a newer queue for the route

	if got := len(api.seen()); got != 0 {
		t.Fatalf("made %d requests, want none for a stale queue", got)
	}
	select {
	case result := <-answer:
		t.Fatalf("stale waiter answered with %+v", result)
	default:
	}
	client.batchMu.Lock()
	still := client.queues[imdbMovies]
	client.batchMu.Unlock()
	if still != current {
		t.Fatal("flushing a stale queue removed the route's current queue")
	}
}

func TestSetUserAgentFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, func(w http.ResponseWriter, req recordedRequest) {
		w.WriteHeader(http.StatusNotFound)
	})
	client := api.client(0)

	for _, tt := range []struct{ set, want string }{
		{set: "  custom/1.0 ", want: "custom/1.0"},
		{set: "   ", want: defaultUserAgent},
	} {
		client.SetUserAgent(tt.set)
		if _, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0000001"); err != nil {
			t.Fatalf("FetchMedia() returned error: %v", err)
		}
		seen := api.seen()
		if got := seen[len(seen)-1].userAgent; got != tt.want {
			t.Fatalf("SetUserAgent(%q): sent %q, want %q", tt.set, got, tt.want)
		}
	}
}

// TestNegativeBatchWindowSendsEachLookupAlone pins that a negative window is
// treated as zero rather than scheduling a timer in the past.
func TestNegativeBatchWindowSendsEachLookupAlone(t *testing.T) {
	t.Parallel()

	api := newScriptedAPI(t, answerEveryID)
	client := api.client(-time.Second)

	client.batchMu.Lock()
	window := client.batchWindow
	client.batchMu.Unlock()
	if window != 0 {
		t.Fatalf("batch window = %v, want 0", window)
	}

	answers := fetchAll(t, client, "imdb", "tt0000001", "tt0000002")
	if imdbRating(answers["tt0000001"]) != 1 || imdbRating(answers["tt0000002"]) != 2 {
		t.Fatalf("answers = %+v, want each title", answers)
	}
	for _, req := range api.seen() {
		if req.method == http.MethodPost {
			t.Fatal("a batch was sent with batching turned off")
		}
	}
}

// sequenceClock answers each call with the next time in its list, repeating
// the last one.
type sequenceClock struct {
	mu    sync.Mutex
	times []time.Time
}

func (c *sequenceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
	}
	return now
}

func TestSendRefusesWithoutARequest(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		endpoint func(serverURL string) string
		setup    func(c *Client)
		want     error
	}{
		{
			name: "already paused",
			ctx:  context.Background(),
			setup: func(c *Client) {
				c.now = func() time.Time { return base }
				c.cooldownUntil = base.Add(time.Hour)
			},
			want: ErrQuotaExhausted,
		},
		{
			name: "pause began while waiting for the limiter",
			ctx:  context.Background(),
			setup: func(c *Client) {
				// Checked before the wait: past the pause. After it: inside.
				c.now = (&sequenceClock{times: []time.Time{base.Add(2 * time.Hour), base}}).Now
				c.cooldownUntil = base.Add(time.Hour)
			},
			want: ErrQuotaExhausted,
		},
		{
			name: "caller gave up before the limiter admitted it",
			ctx:  cancelled,
			want: ErrUnavailable,
		},
		{
			name:     "request cannot be built",
			ctx:      context.Background(),
			endpoint: func(string) string { return "http://bad host/\x7f" },
			want:     ErrUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newScriptedAPI(t, answerEveryID)
			client := api.client(0)
			if tt.setup != nil {
				tt.setup(client)
			}
			endpoint := api.server.URL + "/imdb/movie/tt0000001"
			if tt.endpoint != nil {
				endpoint = tt.endpoint(api.server.URL)
			}

			status, body, err := client.send(tt.ctx, http.MethodGet, endpoint, nil, maxResponseBody, "test")

			if !errors.Is(err, tt.want) {
				t.Fatalf("send() error = %v, want %v", err, tt.want)
			}
			if status != 0 || body != nil {
				t.Fatalf("send() = %d, %q, want no response", status, body)
			}
			if got := len(api.seen()); got != 0 {
				t.Fatalf("made %d requests, want none", got)
			}
		})
	}
}

// TestSendTruncatedBodyIsUnavailable covers a connection that drops mid-body.
func TestSendTruncatedBodyIsUnavailable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"type":"movie"`)
	}))
	t.Cleanup(server.Close)

	client := NewClient()
	client.SetBaseURL(server.URL)
	client.SetAPIKey("secret-key")
	client.SetBatchWindow(0)

	_, err := client.FetchMedia(context.Background(), "imdb", "movie", "tt0000001")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("FetchMedia() error = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error %q leaks the API key", err)
	}
}

func TestRedactKeyAtTheEndOfTheMessage(t *testing.T) {
	t.Parallel()

	got := redact(`dial https://api.mdblist.com/imdb/movie/tt1?apikey=secret`)
	if want := `dial https://api.mdblist.com/imdb/movie/tt1?apikey=REDACTED`; got != want {
		t.Fatalf("redact() = %q, want %q", got, want)
	}
}

func TestObserveQuotaPausesOnlyOnAUsableReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	tests := []struct {
		name       string
		remaining  string
		reset      string
		wantPaused time.Time
	}{
		{name: "no headers"},
		{name: "quota left", remaining: "12", reset: strconv.FormatInt(reset.Unix(), 10)},
		{name: "spent without a reset", remaining: "0"},
		{name: "spent with a zero reset", remaining: "0", reset: "0"},
		{name: "spent with a garbage reset", remaining: "0", reset: "soon"},
		{name: "spent with a reset", remaining: " 0 ", reset: strconv.FormatInt(reset.Unix(), 10), wantPaused: reset},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := NewClient()
			client.now = func() time.Time { return now }
			header := http.Header{}
			if tt.remaining != "" {
				header.Set("X-RateLimit-Remaining", tt.remaining)
			}
			if tt.reset != "" {
				header.Set("X-RateLimit-Reset", tt.reset)
			}

			client.observeQuota(header)

			client.quotaMu.Lock()
			until := client.cooldownUntil
			client.quotaMu.Unlock()
			if !until.Equal(tt.wantPaused) {
				t.Fatalf("paused until %v, want %v", until, tt.wantPaused)
			}
		})
	}
}

func TestRetryAfterIgnoresUnusableValues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"", "0", "-30", "tomorrow", now.Add(-time.Hour).Format(http.TimeFormat)} {
		header := http.Header{}
		header.Set("Retry-After", value)
		if until, ok := retryAfter(header, now); ok {
			t.Fatalf("retryAfter(%q) = %v, want no usable value", value, until)
		}
	}
	if _, ok := retryAfter(nil, now); ok {
		t.Fatal("retryAfter(nil) reported a value")
	}

	header := http.Header{}
	header.Set("Retry-After", " 30 ")
	if until, ok := retryAfter(header, now); !ok || !until.Equal(now.Add(30*time.Second)) {
		t.Fatalf("retryAfter(30) = %v, %v, want now+30s", until, ok)
	}
}

func TestFlexibleIDDecoding(t *testing.T) {
	t.Parallel()

	tests := map[string]flexibleID{
		`null`:       "",
		`578`:        "578",
		`" 578 "`:    "578",
		`5.5`:        "",
		`1e3`:        "",
		`true`:       "",
		`{"id":578}`: "",
	}
	for raw, want := range tests {
		var ids mediaIDs
		if err := json.Unmarshal([]byte(`{"imdb":"tt0073195","tmdb":`+raw+`}`), &ids); err != nil {
			t.Fatalf("tmdb %s: decode failed: %v", raw, err)
		}
		if ids.TMDB != want {
			t.Fatalf("tmdb %s = %q, want %q", raw, ids.TMDB, want)
		}
		if ids.IMDB != "tt0073195" {
			t.Fatalf("tmdb %s: imdb = %q, want the rest of the object kept", raw, ids.IMDB)
		}
	}
}

func TestLabelListTakesNameWhenThereIsNoTitle(t *testing.T) {
	t.Parallel()

	var labels labelList
	if err := json.Unmarshal([]byte(`["Drama",{"title":"Horror"},{"name":"Thriller"},5]`), &labels); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if want := (labelList{"Drama", "Horror", "Thriller"}); !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
}

// TestApplyRatingsFallsBackToValue covers tmdb and tomatoes entries whose
// "score" is null, where the pinned value scale is used instead.
func TestApplyRatingsFallsBackToValue(t *testing.T) {
	t.Parallel()

	value := func(v float64) *float64 { return &v }
	var ratings metadata.Ratings
	applyRatings(&ratings, []ratingEntry{
		{Source: "tmdb", Value: value(76)},
		{Source: "tomatoes", Value: value(96.6)},
	})

	if ratings.TMDB != 7.6 {
		t.Fatalf("TMDB = %v, want 7.6 from value 76", ratings.TMDB)
	}
	if ratings.RTCritic != 97 {
		t.Fatalf("RTCritic = %v, want 97 from value 96.6", ratings.RTCritic)
	}
}
