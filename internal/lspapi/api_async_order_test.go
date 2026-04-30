package lspapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type asyncOrderJSONRPCResponseTestEnvelope struct {
	JSONRPC string                `json:"jsonrpc"`
	ID      any                   `json:"id"`
	Result  AsyncOrderNewResponse `json:"result,omitempty"`
	Error   *AsyncOrderError      `json:"error,omitempty"`
}

func decodeAsyncOrderJSONRPCResponse(t *testing.T, body []byte) asyncOrderJSONRPCResponseTestEnvelope {
	t.Helper()

	var envelope asyncOrderJSONRPCResponseTestEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return envelope
}

func TestInternalAsyncOrderNewRequiresControlToken(t *testing.T) {
	api := &API{
		cfg: Config{
			HTTPTimeout: time.Second,
			// Intentionally leave AsyncOrderBearerToken empty to verify fail-closed behavior.
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/async_order/new", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	api.handleInternalAsyncOrderNew(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestInternalAsyncOrderNewRejectsEmptyPeerPubkey(t *testing.T) {
	api := &API{
		cfg: Config{
			HTTPTimeout:           time.Second,
			AsyncOrderBearerToken: "secret",
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/async_order/new", strings.NewReader(`{"id":"request-1","protocol_version":1,"hashes":[{"hash_index":"1","payment_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()

	api.handleInternalAsyncOrderNew(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeAsyncOrderJSONRPCResponse(t, rr.Body.Bytes())
	if resp.JSONRPC != asyncOrderJSONRPCVersion {
		t.Fatalf("expected jsonrpc %q, got %q", asyncOrderJSONRPCVersion, resp.JSONRPC)
	}
	if resp.ID != "request-1" {
		t.Fatalf("expected id request-1, got %#v", resp.ID)
	}
	if resp.Error == nil {
		t.Fatalf("expected jsonrpc error envelope, got %#v", resp)
	}
	if resp.Error.Code != asyncOrderJSONRPCInvalidRequest {
		t.Fatalf("expected invalid request code %d, got %d", asyncOrderJSONRPCInvalidRequest, resp.Error.Code)
	}
	if resp.Error.Message != "invalid request" {
		t.Fatalf("unexpected error message %q", resp.Error.Message)
	}
}

func TestInternalAsyncOrderNewReturnsJsonRpcEnvelope(t *testing.T) {
	store, err := NewStore(Config{
		DatabaseDriver: "sqlite",
		DatabaseURL:    filepath.Join(t.TempDir(), "async-order.db"),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	api := &API{
		cfg: Config{
			HTTPTimeout:           time.Second,
			AsyncOrderBearerToken: "secret",
		},
		db: store,
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/async_order/new", strings.NewReader(`{"id":"request-2","peer_pubkey":"02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","protocol_version":1,"hashes":[{"hash_index":"1","payment_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"hash_index":"2","payment_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()

	api.handleInternalAsyncOrderNew(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeAsyncOrderJSONRPCResponse(t, rr.Body.Bytes())
	if resp.JSONRPC != asyncOrderJSONRPCVersion {
		t.Fatalf("expected jsonrpc %q, got %q", asyncOrderJSONRPCVersion, resp.JSONRPC)
	}
	if resp.ID != "request-2" {
		t.Fatalf("expected id request-2, got %#v", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("expected no jsonrpc error, got %#v", resp.Error)
	}
	if resp.Result.ProtocolVersion != asyncOrderProtocolVersion {
		t.Fatalf("expected protocol version %d, got %d", asyncOrderProtocolVersion, resp.Result.ProtocolVersion)
	}
	if resp.Result.OrderID != "1" {
		t.Fatalf("expected order id 1, got %q", resp.Result.OrderID)
	}
	if resp.Result.Status != "active" {
		t.Fatalf("expected active status, got %q", resp.Result.Status)
	}
	if resp.Result.AcceptedThroughIndex != "2" {
		t.Fatalf("expected accepted_through_index 2, got %q", resp.Result.AcceptedThroughIndex)
	}
	if resp.Result.NextIndexExpected != "3" {
		t.Fatalf("expected next_index_expected 3, got %q", resp.Result.NextIndexExpected)
	}
	if resp.Result.UnusedHashes != "2" {
		t.Fatalf("expected unused_hashes 2, got %q", resp.Result.UnusedHashes)
	}
	if resp.Result.RefillBatchSize != "200" {
		t.Fatalf("expected refill_batch_size 200, got %q", resp.Result.RefillBatchSize)
	}
}

func TestAsyncOrderHTTPStatusFromErrorCode(t *testing.T) {
	tests := []struct {
		name string
		code int64
		want int
	}{
		{name: "duplicate index", code: asyncOrderErrorDuplicateIndexConflict, want: http.StatusConflict},
		{name: "duplicate hash", code: asyncOrderErrorDuplicateHashConflict, want: http.StatusConflict},
		{name: "internal error", code: asyncOrderJSONRPCInternalError, want: http.StatusInternalServerError},
		{name: "validation error", code: asyncOrderErrorInvalidHashBatch, want: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := asyncOrderHTTPStatusFromErrorCode(tc.code); got != tc.want {
				t.Fatalf("status for code %d = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

func TestAsyncOrderAcceptedThroughIndexSurvivesPoolDeletion(t *testing.T) {
	store, err := NewStore(Config{
		DatabaseDriver: "sqlite",
		DatabaseURL:    filepath.Join(t.TempDir(), "async-order.db"),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	resp, rpcErr, err := store.ApplyAsyncOrderNew(context.Background(), AsyncOrderNewRequest{
		PeerPubkey:      lightningAddressTestPeerPubkey,
		ProtocolVersion: asyncOrderProtocolVersion,
		Hashes: []AsyncOrderNewHashInput{
			{HashIndex: "1", PaymentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{HashIndex: "2", PaymentHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
	})
	if err != nil {
		t.Fatalf("apply async order: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("apply async order rpc error: %+v", rpcErr)
	}
	if resp.AcceptedThroughIndex != "2" {
		t.Fatalf("unexpected accepted_through_index in response: %s", resp.AcceptedThroughIndex)
	}

	orderID, err := strconv.ParseInt(resp.OrderID, 10, 64)
	if err != nil {
		t.Fatalf("parse order id: %v", err)
	}

	var acceptedThroughIndex sql.NullInt64
	if err := store.db.QueryRowContext(context.Background(), `SELECT accepted_through_index FROM async_orders WHERE order_id = ?`, orderID).Scan(&acceptedThroughIndex); err != nil {
		t.Fatalf("lookup accepted_through_index: %v", err)
	}
	if !acceptedThroughIndex.Valid || acceptedThroughIndex.Int64 != 2 {
		t.Fatalf("expected persisted accepted_through_index 2, got %+v", acceptedThroughIndex)
	}

	if _, err := store.db.ExecContext(context.Background(), `DELETE FROM async_hash_pool WHERE order_id = ?`, orderID); err != nil {
		t.Fatalf("delete async hash pool rows: %v", err)
	}

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback()
	})

	snapshot, err := store.asyncOrderSnapshotTx(context.Background(), tx, orderID)
	if err != nil {
		t.Fatalf("snapshot async order: %v", err)
	}
	if snapshot.AcceptedThroughIndex != "2" {
		t.Fatalf("snapshot accepted_through_index = %s, want 2", snapshot.AcceptedThroughIndex)
	}
	if snapshot.NextIndexExpected != "3" {
		t.Fatalf("snapshot next_index_expected = %s, want 3", snapshot.NextIndexExpected)
	}
	if snapshot.UnusedHashes != "0" {
		t.Fatalf("snapshot unused_hashes = %s, want 0", snapshot.UnusedHashes)
	}
}
