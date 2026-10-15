package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writePolicies(t *testing.T, path string, limit int) {
	t.Helper()
	content := fmt.Sprintf("policies:\n  - name: search\n    algorithm: token_bucket\n    limit: %d\n    period: 1m\n", limit)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newLoadedStore(t *testing.T, limit int) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	writePolicies(t, path, limit)
	s := NewStore()
	if err := s.Reload(path); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return s, path
}

func TestStoreEmptyUntilLoaded(t *testing.T) {
	if _, ok := NewStore().Get("search"); ok {
		t.Fatal("empty store found a policy")
	}
}

func TestStoreGetUnknown(t *testing.T) {
	s, _ := newLoadedStore(t, 100)
	if _, ok := s.Get("nope"); ok {
		t.Fatal("found a policy that does not exist")
	}
}

func TestStoreReloadPicksUpChanges(t *testing.T) {
	s, path := newLoadedStore(t, 100)
	if p, _ := s.Get("search"); p.Limit != 100 {
		t.Fatalf("limit = %d, want 100", p.Limit)
	}

	writePolicies(t, path, 7)
	if err := s.Reload(path); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if p, _ := s.Get("search"); p.Limit != 7 {
		t.Fatalf("after reload limit = %d, want 7", p.Limit)
	}
}

func TestStoreFailedReloadKeepsOldPolicies(t *testing.T) {
	s, path := newLoadedStore(t, 100)

	// A typo: strict loading rejects it, so the swap must not happen.
	bad := "policies:\n  - name: search\n    algorithm: token_bucket\n    limt: 5\n    period: 1m\n"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(path); err == nil {
		t.Fatal("Reload of an invalid file succeeded")
	}
	if p, ok := s.Get("search"); !ok || p.Limit != 100 {
		t.Fatalf("after failed reload got %+v (found %v), want the old limit 100", p, ok)
	}

	if err := s.Reload(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("Reload of a missing file succeeded")
	}
	if p, _ := s.Get("search"); p.Limit != 100 {
		t.Fatalf("after missing-file reload limit = %d, want 100", p.Limit)
	}
}

func TestStoreGetReturnsACopy(t *testing.T) {
	s, _ := newLoadedStore(t, 100)
	p, _ := s.Get("search")
	p.Limit = 999
	if again, _ := s.Get("search"); again.Limit != 100 {
		t.Fatalf("changing a returned policy changed the store: limit = %d", again.Limit)
	}
}

// Readers must never see a torn or missing policy while reloads run. Run with
// -race: it is what proves the swap is safe.
func TestStoreConcurrentGetAndReload(t *testing.T) {
	s, path := newLoadedStore(t, 100)

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				p, ok := s.Get("search")
				if !ok || (p.Limit != 100 && p.Limit != 200) {
					t.Errorf("Get = %+v (found %v), want limit 100 or 200", p, ok)
					return
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		writePolicies(t, path, 100+100*(i%2))
		if err := s.Reload(path); err != nil {
			t.Errorf("Reload %d: %v", i, err)
			break
		}
	}
	close(done)
	wg.Wait()
}
