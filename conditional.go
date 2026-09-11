package gofakes3

import (
	"encoding/hex"
	"net/http"
	"strings"
)

func objectETag(obj *Object) string {
	if obj.ETag != "" {
		return obj.ETag
	}
	if len(obj.Hash) != 0 {
		return `"` + hex.EncodeToString(obj.Hash) + `"`
	}
	return ""
}

// matchETag handles quoted tags, including commas inside opaque tags.
// If-Match uses strong comparison; If-None-Match uses weak comparison.
func matchETag(value, current string, exists, weak bool) bool {
	if strings.TrimSpace(value) == "*" {
		return exists
	}
	matched := false
	for {
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			return matched
		}
		tag := value
		isWeak := strings.HasPrefix(value, "W/")
		if isWeak {
			value = value[2:]
		}
		if len(value) == 0 || value[0] != '"' {
			return false
		}
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			return false
		}
		end += 2
		for i := 1; i < end-1; i++ {
			if value[i] < 0x21 || value[i] == 0x7f {
				return false
			}
		}
		quoted := value[:end]
		if exists && current != "" {
			if weak {
				matched = matched || quoted == strings.TrimPrefix(current, "W/")
			} else if !isWeak {
				matched = matched || quoted == current
			}
		}
		consumed := end
		if isWeak {
			consumed += 2
		}
		value = strings.TrimLeft(tag[consumed:], " \t")
		if value != "" && value[0] != ',' {
			return false
		}
	}
}

func checkPreconditions(r *http.Request, obj *Object) error {
	exists := obj != nil && !obj.IsDeleteMarker
	etag := ""
	if exists {
		etag = objectETag(obj)
	}
	_, hasMatch := r.Header["If-Match"]
	if hasMatch {
		if !matchETag(strings.Join(r.Header.Values("If-Match"), ","), etag, exists, false) {
			return ErrPreconditionFailed
		}
	} else if exists {
		if since, err := http.ParseTime(r.Header.Get("If-Unmodified-Since")); err == nil {
			if modified, err := http.ParseTime(obj.Metadata["Last-Modified"]); err == nil && modified.After(since) {
				return ErrPreconditionFailed
			}
		}
	}
	_, hasNoneMatch := r.Header["If-None-Match"]
	if hasNoneMatch {
		if matchETag(strings.Join(r.Header.Values("If-None-Match"), ","), etag, exists, true) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				return ErrNotModified
			}
			return ErrPreconditionFailed
		}
	} else if exists && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if since, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil {
			if modified, err := http.ParseTime(obj.Metadata["Last-Modified"]); err == nil && !modified.After(since) {
				return ErrNotModified
			}
		}
	}
	return nil
}

// The caller holds the object's write lock through the storage mutation.
func (g *GoFakeS3) checkWritePreconditions(bucket, object string, r *http.Request) (err error) {
	_, hasMatch := r.Header["If-Match"]
	_, hasNoneMatch := r.Header["If-None-Match"]
	_, hasUnmodified := r.Header["If-Unmodified-Since"]
	if !hasMatch && !hasNoneMatch && !hasUnmodified {
		return nil
	}
	obj, err := g.storage.HeadObject(r.Context(), bucket, object)
	if HasErrorCode(err, ErrNoSuchKey) {
		return checkPreconditions(r, nil)
	}
	if err != nil {
		return err
	}
	if obj == nil {
		return ErrInternal
	}
	defer CheckClose(obj.Contents, &err)
	return checkPreconditions(r, obj)
}
