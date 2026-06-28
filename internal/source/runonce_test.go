package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"submux/internal/store"
)

func TestRunOnceFetchesOnlyEnabled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte("proxies: []\n"))
	}))
	defer srv.Close()

	st := newTestStore(t)
	enabledID, _ := st.CreateSource(store.Source{Name: "on", URL: srv.URL})
	disabledID, _ := st.CreateSource(store.Source{Name: "off", URL: srv.URL})
	st.SetSourceEnabled(disabledID, false)

	f := NewFetcher(st)
	if err := f.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 fetch (enabled only), got %d", got)
	}
	if _, err := st.GetCache(enabledID); err != nil {
		t.Fatalf("enabled source not cached: %v", err)
	}
	if _, err := st.GetCache(disabledID); err == nil {
		t.Fatalf("disabled source should not be cached")
	}
}

func TestRunOnceContinuesAfterOneError(t *testing.T) {
	st := newTestStore(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("proxies: []\n"))
	}))
	defer good.Close()
	badID, _ := st.CreateSource(store.Source{Name: "bad", URL: "http://127.0.0.1:1/nope"})
	goodID, _ := st.CreateSource(store.Source{Name: "good", URL: good.URL})

	f := NewFetcher(st)
	_ = f.RunOnce(context.Background()) // 不因单源失败而整体中断

	bc, _ := st.GetCache(badID)
	if bc.LastError == "" {
		t.Fatalf("bad source should record error")
	}
	gc, err := st.GetCache(goodID)
	if err != nil || gc.Raw == "" {
		t.Fatalf("good source should still be fetched: %v %+v", err, gc)
	}
}
