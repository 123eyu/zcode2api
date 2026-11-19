package shared

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	googleuuid "github.com/google/uuid"
	"io"
	"maps"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type M = map[string]any

const AccountOperationConcurrency = 3

func Wake(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Callbacks handle cancellation; concurrency stays bounded regardless of count.
func ParallelFor(count, concurrency int, work func(int)) {
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(count, max(1, concurrency)) {
		workers.Go(func() {
			for i := int(next.Add(1) - 1); i < count; i = int(next.Add(1) - 1) {
				work(i)
			}
		})
	}
	workers.Wait()
}

func Obj(v any) M {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return M{}
}

func Arr(v any) []any {
	if a, ok := v.([]any); ok && a != nil {
		return a
	}
	return []any{}
}

func Str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func Num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case bool:
		if n {
			return 1
		}
		return 0
	default:
		f, _ := strconv.ParseFloat(Str(v), 64)
		return f
	}
}

func Integer(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		if i, e := n.Int64(); e == nil {
			return i
		}
	}
	return int64(Num(v))
}

func IntegerInRange(v any, minimum, maximum float64) bool {
	switch v.(type) {
	case int, int64, float64, json.Number:
		n := Num(v)
		return n >= minimum && n <= maximum && n == math.Trunc(n)
	}
	return false
}

func Truth(v any) bool {
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case map[string]any, []any:
		return true
	default:
		return Num(v) != 0
	}
}

func First(v ...any) any {
	for _, x := range v {
		if x != nil {
			return x
		}
	}
	return nil
}

func Fallback(v any, d any) any {
	if Truth(v) {
		return v
	}
	return d
}

func JSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func Parse(s any, d any) any {
	var v any
	decoder := json.NewDecoder(strings.NewReader(Str(s)))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return d
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return d
	}
	return v
}

func CloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return Clone(x)
	case []any:
		if x == nil {
			return []any(nil)
		}
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = CloneValue(v)
		}
		return out
	default:
		return v
	}
}

func Clone(m M) M {
	out := make(M, len(m))
	for k, v := range m {
		out[k] = CloneValue(v)
	}
	return out
}

func Merge(a, b M) M {
	c := M{}
	maps.Copy(c, a)
	maps.Copy(c, b)
	return c
}

func CopyFields(dst, src M, keys ...string) {
	for _, key := range keys {
		if value, present := src[key]; present {
			dst[key] = value
		}
	}
}

func NowMS() int64 { return time.Now().UnixMilli() }

func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func Random(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

func UUID() string { return googleuuid.NewString() }

func UUIDv7() string { return googleuuid.Must(googleuuid.NewV7()).String() }

func Contains(a []any, s string) bool {
	for _, v := range a {
		if Str(v) == s {
			return true
		}
	}
	return false
}

func Lower(s any) string { return strings.ToLower(Str(s)) }

func CodeIs(v any, n int) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return Num(v) == float64(n)
	}
	return false
}

func Ms(n any) time.Duration { return time.Duration(Num(n) * float64(time.Millisecond)) }

func Nullable(v any) any {
	if v == nil || Str(v) == "" {
		return nil
	}
	return v
}
