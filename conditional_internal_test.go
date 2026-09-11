package gofakes3

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestETagMatching(t *testing.T) {
	for _, tc := range []struct {
		value   string
		current string
		exists  bool
		weak    bool
		want    bool
	}{
		{"*", "", true, false, true},
		{"*", "", false, false, false},
		{`"a,b", "c"`, `"a,b"`, true, false, true},
		{`W/"a"`, `"a"`, true, false, false},
		{`W/"a"`, `"a"`, true, true, true},
		{`"a"`, `W/"a"`, true, false, false},
		{`"a"`, `W/"a"`, true, true, true},
		{`, "a",,`, `"a"`, true, false, true},
		{`"a" "b"`, `"a"`, true, false, false},
		{`"a", *`, `"a"`, true, false, false},
		{`"unterminated`, `"a"`, true, false, false},
		{`""`, `""`, true, false, true},
		{`""`, "", true, false, false},
		{`"a"`, `"a"`, false, false, false},
	} {
		if got := matchETag(tc.value, tc.current, tc.exists, tc.weak); got != tc.want {
			t.Errorf("matchETag(%q, %q, %t, %t) = %t, want %t",
				tc.value, tc.current, tc.exists, tc.weak, got, tc.want)
		}
	}
}

func TestPreconditionOrder(t *testing.T) {
	modified := time.Unix(1445412480, 0).UTC()
	past := modified.Add(-time.Hour).Format(http.TimeFormat)
	future := modified.Add(time.Hour).Format(http.TimeFormat)
	obj := &Object{ETag: `"current"`, Metadata: map[string]string{"Last-Modified": modified.Format(http.TimeFormat)}}
	for _, tc := range []struct {
		name    string
		method  string
		headers http.Header
		want    error
	}{
		{"unmodified fails", "PUT", http.Header{"If-Unmodified-Since": {past}}, ErrPreconditionFailed},
		{"unmodified passes", "PUT", http.Header{"If-Unmodified-Since": {future}}, nil},
		{"match suppresses date", "PUT", http.Header{"If-Match": {`"current"`}, "If-Unmodified-Since": {past}}, nil},
		{"match precedes none", "GET", http.Header{"If-Match": {`"wrong"`}, "If-None-Match": {"*"}}, ErrPreconditionFailed},
		{"none precedes modified", "GET", http.Header{"If-None-Match": {`"other"`}, "If-Modified-Since": {future}}, nil},
		{"not modified", "GET", http.Header{"If-Modified-Since": {future}}, ErrNotModified},
		{"modified", "GET", http.Header{"If-Modified-Since": {past}}, nil},
		{"put ignores modified", "PUT", http.Header{"If-Modified-Since": {future}}, nil},
		{"invalid date ignored", "PUT", http.Header{"If-Unmodified-Since": {"invalid"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/bucket/object", nil)
			r.Header = tc.headers
			if got := checkPreconditions(r, obj); got != tc.want {
				t.Fatalf("error = %v, want %v", got, tc.want)
			}
		})
	}
	for _, obj := range []*Object{nil, {IsDeleteMarker: true}} {
		r := httptest.NewRequest("PUT", "/bucket/object", nil)
		r.Header.Set("If-Match", "*")
		if checkPreconditions(r, obj) != ErrPreconditionFailed {
			t.Fatal("missing object matched wildcard")
		}
		r.Header.Del("If-Match")
		r.Header.Set("If-None-Match", "*")
		if err := checkPreconditions(r, obj); err != nil {
			t.Fatalf("missing object failed create condition: %v", err)
		}
	}
}

func TestResponseETagAndPreconditionHeaders(t *testing.T) {
	g := &GoFakeS3{log: DiscardLog()}
	obj := &Object{ETag: `"multipart-2"`, Hash: []byte{1},
		Metadata: map[string]string{"Content-Length": "999", "Content-Type": "text/plain"}}
	r := httptest.NewRequest("GET", "/bucket/object", nil)
	r.Header.Set("If-Match", `"wrong"`)
	w := httptest.NewRecorder()
	err := g.writeGetOrHeadObjectResponse(obj, w, r)
	if err != ErrPreconditionFailed {
		t.Fatalf("error = %v", err)
	}
	g.httpError(w, r, err)
	if w.Code != 412 || w.Header().Get("ETag") != obj.ETag || w.Header().Get("Content-Length") != "" {
		t.Fatalf("invalid conditional response: %d %v", w.Code, w.Header())
	}
	if objectETag(&Object{}) != "" {
		t.Fatal("missing hash became a usable validator")
	}
}

func TestObjectLocksReleaseEntries(t *testing.T) {
	var locks objectLocks
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := locks.lock("bucket", true, "second", "first", "second")
			unlock()
		}()
	}
	wg.Wait()
	if len(locks.entries) != 0 {
		t.Fatalf("retained %d lock entries", len(locks.entries))
	}
}
