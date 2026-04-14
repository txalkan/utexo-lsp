package lspapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestLightningAddressAccountMintedOnceAndPersisted(t *testing.T) {
	store, err := NewStore(Config{
		DatabaseDriver: "sqlite",
		DatabaseURL:    filepath.Join(t.TempDir(), "lnaddr.db"),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	api := &API{db: store}

	peerPubkey := "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	account1, err := api.ensureLightningAddressAccount(context.Background(), peerPubkey)
	if err != nil {
		t.Fatalf("ensure first account: %v", err)
	}
	account2, err := api.ensureLightningAddressAccount(context.Background(), peerPubkey)
	if err != nil {
		t.Fatalf("ensure second account: %v", err)
	}

	if account1.Username == "" {
		t.Fatalf("expected minted account handle, got %+v", account1)
	}
	if account1.Username != account2.Username {
		t.Fatalf("expected persisted handle after first insert, got %+v and %+v", account1, account2)
	}

	gotByPeer, err := store.GetLightningAddressAccountByPeerPubkey(context.Background(), strings.ToLower(peerPubkey))
	if err != nil {
		t.Fatalf("lookup by peer pubkey: %v", err)
	}
	if gotByPeer.Username != account1.Username {
		t.Fatalf("unexpected stored account: %+v vs %+v", gotByPeer, account1)
	}
}

func TestLightningAddressDiscoveryUsesDbBackedAccount(t *testing.T) {
	store, err := NewStore(Config{
		DatabaseDriver: "sqlite",
		DatabaseURL:    filepath.Join(t.TempDir(), "lnaddr.db"),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	api := &API{
		cfg: Config{
			LightningAddressDomainURL:        "https://example.com",
			LightningAddressShortDescription: "Payment to example",
			LightningAddressMinSendableMsat:  1_000,
			LightningAddressMaxSendableMsat:  5_000,
		},
		db: store,
	}

	peerPubkey := "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	account, err := api.ensureLightningAddressAccount(context.Background(), peerPubkey)
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}

	gotByPeer, err := store.GetLightningAddressAccountByPeerPubkey(context.Background(), strings.ToLower(peerPubkey))
	if err != nil {
		t.Fatalf("lookup by peer pubkey: %v", err)
	}
	if gotByPeer.Username != account.Username {
		t.Fatalf("unexpected stored account: %+v vs %+v", gotByPeer, account)
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/lnurlp/"+account.Username, nil)
	rr := httptest.NewRecorder()

	api.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp LightningAddressDiscoveryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	expectedCallback := "https://example.com/pay/callback/" + url.PathEscape(account.Username)
	if resp.Callback != expectedCallback {
		t.Fatalf("unexpected callback: %s", resp.Callback)
	}

	expectedMetadata := `[["text/identifier","` + account.Username + `@example.com"],["text/plain","Payment to example"]]`
	if resp.Metadata != expectedMetadata {
		t.Fatalf("unexpected metadata: %s", resp.Metadata)
	}
}

func TestLightningAddressDiscoveryRejectsSuffix(t *testing.T) {
	api, account := newLightningAddressTestAPI(t, "https://example.com", "Payment to txalkan", nil)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/lnurlp/"+account.Username+"+tips", nil)
	rr := httptest.NewRecorder()

	api.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}
