package clusters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kubemap/portal/store"
)

func TestListAndGet(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnsureCluster(ctx, "local", "this cluster"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(Config{Store: st}).Routes(func(h http.Handler) http.Handler { return h })(mux)

	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	code, m := get("/api/clusters")
	cs, _ := m["clusters"].([]any)
	if code != 200 || len(cs) != 1 || cs[0].(map[string]any)["id"] != "local" || cs[0].(map[string]any)["status"] != store.StatusActive {
		t.Fatalf("list = %d %v", code, m)
	}
	if code, m := get("/api/clusters/local"); code != 200 || m["cluster"].(map[string]any)["name"] != "this cluster" {
		t.Errorf("get = %d %v", code, m)
	}
	if code, _ := get("/api/clusters/nope"); code != 404 {
		t.Errorf("unknown cluster: %d", code)
	}
}
