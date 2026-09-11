package gofakes3_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/gofakes3"
	"github.com/OpenListTeam/gofakes3/s3mem"
)

func conditionalRequest(h http.Handler, method, target, body string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	for key, values := range headers {
		r.Header[key] = values
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func conditionalServer(t *testing.T, backend gofakes3.Backend) http.Handler {
	t.Helper()
	if err := backend.CreateBucket(context.Background(), "bucket"); err != nil {
		t.Fatal(err)
	}
	return gofakes3.New(backend, gofakes3.WithTimeSkewLimit(0)).Server()
}

func TestConditionalPut(t *testing.T) {
	oldTag := fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum([]byte("old"))))
	for _, tc := range []struct {
		name    string
		exists  bool
		headers http.Header
		status  int
	}{
		{"create only missing", false, http.Header{"If-None-Match": {"*"}}, 200},
		{"create only existing", true, http.Header{"If-None-Match": {"*"}}, 412},
		{"missing match", false, http.Header{"If-Match": {oldTag}}, 412},
		{"missing wildcard", false, http.Header{"If-Match": {"*"}}, 412},
		{"wrong match", true, http.Header{"If-Match": {`"wrong"`}}, 412},
		{"current match", true, http.Header{"If-Match": {oldTag}}, 200},
		{"existing wildcard", true, http.Header{"If-Match": {"*"}}, 200},
		{"weak match rejected", true, http.Header{"If-Match": {"W/" + oldTag}}, 412},
		{"weak none match", true, http.Header{"If-None-Match": {"W/" + oldTag}}, 412},
		{"tag list", true, http.Header{"If-Match": {`"wrong", ` + oldTag}}, 200},
		{"repeated header", true, http.Header{"If-Match": {`"wrong"`, oldTag}}, 200},
		{"both fail", true, http.Header{"If-Match": {`"wrong"`}, "If-None-Match": {"*"}}, 412},
		{"unconditional", true, nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := conditionalServer(t, s3mem.New())
			if tc.exists {
				w := conditionalRequest(h, "PUT", "/bucket/object", "old", nil)
				if w.Code != 200 || w.Header().Get("ETag") != oldTag {
					t.Fatalf("initial PUT: %d %s", w.Code, w.Body.String())
				}
			}
			w := conditionalRequest(h, "PUT", "/bucket/object", "new", tc.headers)
			if w.Code != tc.status {
				t.Fatalf("PUT status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if w.Code == 412 && !strings.Contains(w.Body.String(), "<Code>PreconditionFailed</Code>") {
				t.Fatalf("missing S3 error code: %s", w.Body.String())
			}
			got := conditionalRequest(h, "GET", "/bucket/object", "", nil)
			if tc.status == 200 {
				if got.Code != 200 || got.Body.String() != "new" {
					t.Fatalf("successful write: %d %q", got.Code, got.Body.String())
				}
				if got.Header().Get("ETag") != w.Header().Get("ETag") {
					t.Fatal("PUT and GET ETags differ")
				}
			} else if tc.exists {
				if got.Code != 200 || got.Body.String() != "old" {
					t.Fatalf("failed condition changed the object: %d %q", got.Code, got.Body.String())
				}
			} else if got.Code != 404 {
				t.Fatalf("failed condition created the object: %d", got.Code)
			}
		})
	}
}

func TestConditionalReadsAndStalePut(t *testing.T) {
	h := conditionalServer(t, s3mem.New())
	put := conditionalRequest(h, "PUT", "/bucket/object", "old", nil)
	oldTag := put.Header().Get("ETag")
	for _, method := range []string{"GET", "HEAD"} {
		for _, tc := range []struct {
			header string
			value  string
			status int
		}{
			{"If-None-Match", "*", 304},
			{"If-None-Match", "W/" + oldTag, 304},
			{"If-None-Match", `"wrong", ` + oldTag, 304},
			{"If-Match", `"wrong"`, 412},
			{"If-Match", oldTag, 200},
		} {
			w := conditionalRequest(h, method, "/bucket/object", "", http.Header{tc.header: {tc.value}})
			if w.Code != tc.status {
				t.Fatalf("%s %s: status %d, want %d", method, tc.header, w.Code, tc.status)
			}
			if (method == "HEAD" || tc.status == 304) && w.Body.Len() != 0 {
				t.Fatalf("%s returned a body with status %d", method, tc.status)
			}
			if w.Header().Get("ETag") != oldTag {
				t.Fatalf("%s lost the validator", method)
			}
		}
	}
	put = conditionalRequest(h, "PUT", "/bucket/object", "new", http.Header{"If-Match": {oldTag}})
	if put.Code != 200 || put.Header().Get("ETag") == oldTag {
		t.Fatal("conditional update did not create a new validator")
	}
	stale := conditionalRequest(h, "PUT", "/bucket/object", "bad", http.Header{"If-Match": {oldTag}})
	if stale.Code != 412 {
		t.Fatalf("stale update returned %d", stale.Code)
	}
	missingBucket := conditionalRequest(h, "PUT", "/absent/object", "bad", http.Header{"If-Match": {"*"}})
	if missingBucket.Code != 404 {
		t.Fatalf("missing bucket returned %d", missingBucket.Code)
	}
}

func TestConcurrentConditionalPut(t *testing.T) {
	for _, header := range []string{"If-None-Match", "If-Match"} {
		t.Run(header, func(t *testing.T) {
			h := conditionalServer(t, s3mem.New())
			value := "*"
			if header == "If-Match" {
				value = conditionalRequest(h, "PUT", "/bucket/object", "initial", nil).Header().Get("ETag")
			}
			const count = 32
			results := make(chan int, count)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range count {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results <- conditionalRequest(h, "PUT", "/bucket/object", fmt.Sprintf("value-%d", i),
						http.Header{header: {value}}).Code
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			success := 0
			for status := range results {
				if status == 200 {
					success++
				} else if status != 412 {
					t.Fatalf("unexpected status %d", status)
				}
			}
			if success != 1 {
				t.Fatalf("%d concurrent writes succeeded, want 1", success)
			}
		})
	}
}

type conditionalHeadErrorBackend struct {
	gofakes3.Backend
}

func (b conditionalHeadErrorBackend) HeadObject(context.Context, string, string) (*gofakes3.Object, error) {
	return nil, gofakes3.ErrInternal
}

func TestConditionalPutPropagatesBackendError(t *testing.T) {
	h := conditionalServer(t, conditionalHeadErrorBackend{s3mem.New()})
	w := conditionalRequest(h, "PUT", "/bucket/object", "new", http.Header{"If-None-Match": {"*"}})
	if w.Code != 500 {
		t.Fatalf("backend error treated as absence: %d", w.Code)
	}
}

type conditionalMultipartBackend struct {
	*s3mem.Backend
	part []byte
}

func (b *conditionalMultipartBackend) CreateMultipartUpload(context.Context, string, string, map[string]string) (gofakes3.UploadID, error) {
	return "upload", nil
}

func (b *conditionalMultipartBackend) UploadPart(_ context.Context, _, _ string, _ gofakes3.UploadID, _ int, _ int64, body io.Reader) (string, error) {
	var err error
	b.part, err = io.ReadAll(body)
	return fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum(b.part))), err
}

func (b *conditionalMultipartBackend) CompleteMultipartUpload(ctx context.Context, bucket, object string, _ gofakes3.UploadID, _ *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	_, err := b.PutObject(ctx, bucket, object, map[string]string{}, bytes.NewReader(b.part), int64(len(b.part)))
	return "", fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum(b.part))), err
}

func (b *conditionalMultipartBackend) AbortMultipartUpload(context.Context, string, string, gofakes3.UploadID) error {
	return nil
}

func TestConditionalMultipartCompletion(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			var backend gofakes3.Backend = s3mem.New()
			if streaming {
				backend = &conditionalMultipartBackend{Backend: s3mem.New()}
			}
			h := conditionalServer(t, backend)
			old := conditionalRequest(h, "PUT", "/bucket/object", "old", nil)
			begin := conditionalRequest(h, "POST", "/bucket/object?uploads", "", nil)
			var upload struct {
				UploadID string `xml:"UploadId"`
			}
			if err := xml.Unmarshal(begin.Body.Bytes(), &upload); err != nil || upload.UploadID == "" {
				t.Fatalf("begin multipart: %d %s", begin.Code, begin.Body.String())
			}
			target := "/bucket/object?uploadId=" + upload.UploadID
			part := conditionalRequest(h, "PUT", target+"&partNumber=1", "new", nil)
			body := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" +
				part.Header().Get("ETag") + "</ETag></Part></CompleteMultipartUpload>"
			failed := conditionalRequest(h, "POST", target, body, http.Header{"If-None-Match": {"*"}})
			if failed.Code != 412 {
				t.Fatalf("conditional completion returned %d: %s", failed.Code, failed.Body.String())
			}
			if got := conditionalRequest(h, "GET", "/bucket/object", "", nil); got.Body.String() != "old" {
				t.Fatal("failed completion overwrote the object")
			}
			retry := conditionalRequest(h, "POST", target, body, http.Header{"If-Match": {old.Header().Get("ETag")}})
			if retry.Code != 200 {
				t.Fatalf("retry lost the upload: %d %s", retry.Code, retry.Body.String())
			}
			if got := conditionalRequest(h, "GET", "/bucket/object", "", nil); got.Body.String() != "new" {
				t.Fatal("successful retry did not store the object")
			}
		})
	}
}

func TestConditionalCopyDestination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exists bool
		header string
		tag    string
		status int
	}{
		{"create absent", false, "If-None-Match", "*", 200},
		{"create existing", true, "If-None-Match", "*", 412},
		{"match absent", false, "If-Match", "*", 412},
		{"match current destination", true, "If-Match", "destination", 200},
		{"match source is insufficient", true, "If-Match", "source", 412},
		{"wrong destination", true, "If-Match", `"wrong"`, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := conditionalServer(t, s3mem.New())
			source := conditionalRequest(h, "PUT", "/bucket/source", "source", nil)
			destinationTag := ""
			if tc.exists {
				destinationTag = conditionalRequest(h, "PUT", "/bucket/destination", "destination", nil).Header().Get("ETag")
			}
			tag := tc.tag
			if tag == "destination" {
				tag = destinationTag
			} else if tag == "source" {
				tag = source.Header().Get("ETag")
			}
			headers := http.Header{"X-Amz-Copy-Source": {"/bucket/source"}, tc.header: {tag}}
			w := conditionalRequest(h, "PUT", "/bucket/destination", "", headers)
			if w.Code != tc.status {
				t.Fatalf("copy status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			got := conditionalRequest(h, "GET", "/bucket/destination", "", nil)
			if tc.status == 200 {
				if got.Code != 200 || got.Body.String() != "source" {
					t.Fatalf("copy result: %d %q", got.Code, got.Body.String())
				}
				if tc.exists {
					stale := conditionalRequest(h, "PUT", "/bucket/destination", "", headers)
					if stale.Code != 412 {
						t.Fatalf("stale copy returned %d", stale.Code)
					}
				}
			} else if tc.exists {
				if got.Body.String() != "destination" {
					t.Fatal("failed copy overwrote the destination")
				}
			} else if got.Code != 404 {
				t.Fatal("failed copy created the destination")
			}
		})
	}
}

func TestConcurrentConditionalCopy(t *testing.T) {
	h := conditionalServer(t, s3mem.New())
	conditionalRequest(h, "PUT", "/bucket/source", "source", nil)
	const count = 32
	results := make(chan int, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- conditionalRequest(h, "PUT", "/bucket/destination", "",
				http.Header{"X-Amz-Copy-Source": {"/bucket/source"}, "If-None-Match": {"*"}}).Code
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for status := range results {
		if status == 200 {
			success++
		} else if status != 412 {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if success != 1 {
		t.Fatalf("%d concurrent copies succeeded, want 1", success)
	}
}
