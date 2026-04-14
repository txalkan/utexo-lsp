package lspapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const lightningAddressTestPeerPubkey = "03aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newLightningAddressTestAPI(t *testing.T, domainURL, shortDescription string, lspClient *NodeClient) (*API, LightningAddressAccount) {
	t.Helper()

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
			LightningAddressDomainURL:        domainURL,
			LightningAddressShortDescription: shortDescription,
			LightningAddressMinSendableMsat:  1_000,
			LightningAddressMaxSendableMsat:  5_000,
			LightningAddressInvoiceExpiry:    time.Hour,
		},
		db:        store,
		lspClient: lspClient,
	}

	account, err := api.ensureLightningAddressAccount(context.Background(), lightningAddressTestPeerPubkey)
	if err != nil {
		t.Fatalf("ensure lightning address account: %v", err)
	}
	t.Logf("minted lightning address username: %s", account.Username)

	return api, account
}

func TestLightningAddressDiscovery(t *testing.T) {
	api, account := newLightningAddressTestAPI(t, "https://example.com", "Payment to txalkan", nil)

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

	if resp.Callback != "https://example.com/pay/callback/"+url.PathEscape(account.Username) {
		t.Fatalf("unexpected callback: %s", resp.Callback)
	}
	if resp.Tag != "payRequest" {
		t.Fatalf("unexpected tag: %s", resp.Tag)
	}
	if resp.MinSendable != 1_000 || resp.MaxSendable != 5_000 {
		t.Fatalf("unexpected sendable range: %+v", resp)
	}

	wantMetadata := `[["text/identifier","` + account.Username + `@example.com"],["text/plain","Payment to txalkan"]]`
	if resp.Metadata != wantMetadata {
		t.Fatalf("unexpected metadata: %s", resp.Metadata)
	}
}

func TestLightningAddressDiscoveryRejectsDomainPath(t *testing.T) {
	api, account := newLightningAddressTestAPI(t, "https://example.com/app", "Payment to txalkan", nil)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/lnurlp/"+account.Username, nil)
	rr := httptest.NewRecorder()

	api.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "path is not allowed") {
		t.Fatalf("unexpected response body: %s", rr.Body.String())
	}
}

func TestLightningAddressCallbackIncludesDescriptionHash(t *testing.T) {
	var received map[string]any

	api, account := newLightningAddressTestAPI(t, "https://example.com", "Payment to txalkan", newInvoiceStubClient(t, &received, http.StatusOK, map[string]string{"invoice": "lnbc1testinvoice"}))
	api.cfg.LNInvoicePath = "/lninvoice"

	req := httptest.NewRequest(http.MethodGet, "/pay/callback/"+url.PathEscape(account.Username)+"?amount=3000", nil)
	rr := httptest.NewRecorder()

	api.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp LightningAddressCallbackResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.PR != "lnbc1testinvoice" {
		t.Fatalf("unexpected invoice: %s", resp.PR)
	}
	t.Logf("minted lightning address invoice: %s", resp.PR)
	if len(resp.Routes) != 0 {
		t.Fatalf("expected empty routes, got %#v", resp.Routes)
	}
}

func TestLightningAddressCallbackFailsIfDescriptionHashRejected(t *testing.T) {
	var requestCount atomic.Int32

	api, account := newLightningAddressTestAPI(t, "https://example.com", "Payment to txalkan", newInvoiceStubClient(t, nil, http.StatusBadRequest, map[string]string{"error": "description_hash unsupported"}, &requestCount))
	api.cfg.LNInvoicePath = "/lninvoice"

	req := httptest.NewRequest(http.MethodGet, "/pay/callback/"+url.PathEscape(account.Username)+"?amount=3000", nil)
	rr := httptest.NewRecorder()

	api.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if requestCount.Load() != 1 {
		t.Fatalf("expected 1 request and no fallback, got %d", requestCount.Load())
	}
	if !strings.Contains(rr.Body.String(), "error constructing invoice") {
		t.Fatalf("unexpected response body: %s", rr.Body.String())
	}
}

type invoiceStubRoundTripper struct {
	t            *testing.T
	received     *map[string]any
	statusCode   int
	responseBody any
	requestCount *atomic.Int32
}

func (rt *invoiceStubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.requestCount != nil {
		rt.requestCount.Add(1)
	}
	if req.Method != http.MethodPost {
		rt.t.Errorf("unexpected method: %s", req.Method)
	}
	if req.URL.Path != "/lninvoice" {
		rt.t.Errorf("unexpected path: %s", req.URL.Path)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		rt.t.Errorf("read body: %v", err)
	}
	if rt.received != nil {
		if err := json.Unmarshal(body, rt.received); err != nil {
			rt.t.Errorf("unmarshal body: %v", err)
		}
		if _, ok := (*rt.received)["description_hash"]; !ok {
			rt.t.Errorf("expected description_hash in request body: %s", string(body))
		}
	}

	buf, err := json.Marshal(rt.responseBody)
	if err != nil {
		rt.t.Errorf("marshal response: %v", err)
	}

	return &http.Response{
		StatusCode: rt.statusCode,
		Status:     http.StatusText(rt.statusCode),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(buf))),
		Request:    req,
	}, nil
}

func newInvoiceStubClient(t *testing.T, received *map[string]any, statusCode int, responseBody any, requestCount ...*atomic.Int32) *NodeClient {
	t.Helper()

	var counter *atomic.Int32
	if len(requestCount) > 0 {
		counter = requestCount[0]
	}

	return &NodeClient{
		baseURL: "http://invoice-stub",
		http: &http.Client{
			Transport: &invoiceStubRoundTripper{
				t:            t,
				received:     received,
				statusCode:   statusCode,
				responseBody: responseBody,
				requestCount: counter,
			},
		},
	}
}
