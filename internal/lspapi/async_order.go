package lspapi

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	asyncOrderStatusActive     = "active"
	asyncPoolStatusAvailable   = "available"
	asyncPoolStatusReserved    = "reserved"
	asyncPoolStatusConsumed    = "consumed"
	asyncInvoiceStatusReserved = "reserved"
	asyncInvoiceStatusActive   = "active"
	asyncInvoiceStatusFailed   = "failed"
)

var errAsyncOrderNotFound = errors.New("async order not found")
var errAsyncHashPoolEmpty = errors.New("async hash pool is empty")
var errAsyncInvoiceNotFound = errors.New("async rotating invoice not found")
var errAsyncRotatingInvoiceAmountMsatMismatch = errors.New("async rotating invoice amount_msat mismatch")
var errAsyncRotatingInvoiceInvalidAmountMsat = errors.New("async rotating invoice invalid amount_msat")
var errAsyncRotatingInvoiceAssetAmountMismatch = errors.New("async rotating invoice asset_amount mismatch")
var errAsyncRotatingInvoiceAssetIDMismatch = errors.New("async rotating invoice asset_id mismatch")
var errAsyncRotatingInvoiceInvalidAssetAmount = errors.New("async rotating invoice invalid asset_amount")
var errAsyncRotatingInvoiceInvalidAssetID = errors.New("async rotating invoice invalid asset_id")

type asyncOrderRow struct {
	OrderID              int64
	PeerPubkey           string
	Status               string
	AcceptedThroughIndex sql.NullInt64
	CurrentInvoiceSlot   sql.NullInt64
	CurrentHashIndex     sql.NullInt64
	CurrentPaymentHash   sql.NullString
	CurrentInvoiceID     sql.NullInt64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type asyncHashPoolRow struct {
	ID          int64
	OrderID     int64
	HashIndex   int64
	PaymentHash string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type asyncRotatingInvoiceRow struct {
	ID            int64
	OrderID       int64
	InvoiceSlot   int64
	HashIndex     int64
	PaymentHash   string
	AssetAmount   sql.NullInt64
	AssetID       sql.NullString
	InvoiceString sql.NullString
	AmountMsat    uint64
	ExpiresAt     time.Time
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type parsedAsyncOrderHash struct {
	HashIndex   int64
	PaymentHash string
}

func (s *SQLStore) inDBTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return nil
}

func (s *SQLStore) ReserveLightningAddressInvoiceSlot(ctx context.Context, account LightningAddressAccount, amountMsat uint64, assetID *string, assetAmount *uint64, expiry time.Duration) (AsyncRotatingInvoice, error) {
	account.PeerPubkey = normalizePeerPubkey(account.PeerPubkey)
	if account.PeerPubkey == "" {
		return AsyncRotatingInvoice{}, errors.New("empty peer_pubkey")
	}
	if expiry <= 0 {
		expiry = time.Hour
	}

	var reserved AsyncRotatingInvoice
	err := s.inDBTx(ctx, func(tx *sql.Tx) error {
		order, err := s.bootstrapAsyncOrderTx(ctx, tx, account.PeerPubkey)
		if err != nil {
			return err
		}
		invoiceSlot := nextInt64FromCurrent(order.CurrentInvoiceSlot)

		available, err := s.countAvailableAsyncHashPoolTx(ctx, tx, order.OrderID)
		if err != nil {
			return err
		}
		if available == 0 {
			return errAsyncHashPoolEmpty
		}

		poolEntry, err := s.reserveAsyncHashPoolEntryTx(ctx, tx, order.OrderID)
		if err != nil {
			return err
		}

		reserved = AsyncRotatingInvoice{
			OrderID:     order.OrderID,
			InvoiceSlot: invoiceSlot,
			HashIndex:   poolEntry.HashIndex,
			PaymentHash: poolEntry.PaymentHash,
			AmountMsat:  amountMsat,
			AssetAmount: assetAmount,
			AssetID:     assetID,
			ExpiresAt:   time.Now().UTC().Add(expiry),
			Status:      asyncInvoiceStatusReserved,
		}

		id, err := s.insertAsyncRotatingInvoiceTx(ctx, tx, reserved)
		if err != nil {
			return err
		}
		reserved.ID = id

		return nil
	})
	return reserved, err
}

func (s *SQLStore) FinalizeLightningAddressInvoiceSlot(ctx context.Context, reservationID int64, invoice string) error {
	invoice = strings.TrimSpace(invoice)
	if invoice == "" {
		return errors.New("empty invoice")
	}

	return s.inDBTx(ctx, func(tx *sql.Tx) error {
		rec, err := s.loadAsyncRotatingInvoiceTx(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if rec.Status == asyncInvoiceStatusFailed {
			return fmt.Errorf("async rotating invoice %d already failed", reservationID)
		}
		if rec.InvoiceString.Valid && rec.InvoiceString.String != "" && rec.InvoiceString.String != invoice && rec.Status == asyncInvoiceStatusActive {
			return fmt.Errorf("async rotating invoice %d already finalized with a different invoice", reservationID)
		}

		if err := s.finalizeAsyncRotatingInvoiceTx(ctx, tx, reservationID, invoice); err != nil {
			return err
		}
		if err := s.consumeAsyncHashPoolEntryTx(ctx, tx, rec.OrderID, rec.HashIndex); err != nil {
			return err
		}
		return s.updateAsyncOrderCurrentInvoiceTx(ctx, tx, rec.OrderID, reservationID, rec.InvoiceSlot, rec.HashIndex, rec.PaymentHash)
	})
}

func (s *SQLStore) ReleaseLightningAddressInvoiceSlot(ctx context.Context, reservationID int64, lastErr string) error {
	_ = lastErr
	return s.inDBTx(ctx, func(tx *sql.Tx) error {
		rec, err := s.loadAsyncRotatingInvoiceTx(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if rec.Status == asyncInvoiceStatusActive {
			return fmt.Errorf("async rotating invoice %d is already active", reservationID)
		}
		if rec.Status == asyncInvoiceStatusFailed {
			return nil
		}

		if err := s.markAsyncRotatingInvoiceFailedTx(ctx, tx, reservationID); err != nil {
			return err
		}
		return s.releaseAsyncHashPoolEntryTx(ctx, tx, rec.OrderID, rec.HashIndex)
	})
}

func (s *SQLStore) ApplyAsyncOrderNew(ctx context.Context, req AsyncOrderNewRequest) (AsyncOrderNewResponse, *AsyncOrderError, error) {
	req.PeerPubkey = normalizePeerPubkey(req.PeerPubkey)
	if req.PeerPubkey == "" {
		return AsyncOrderNewResponse{}, asyncOrderInvalidHashBatch(), nil
	}
	if req.ProtocolVersion != asyncOrderProtocolVersion {
		return AsyncOrderNewResponse{}, &AsyncOrderError{
			Code:    asyncOrderErrorUnsupportedProtocolVersion,
			Message: "unsupported_protocol_version",
		}, nil
	}
	if len(req.Hashes) == 0 || len(req.Hashes) > asyncHashPoolMaxSize {
		return AsyncOrderNewResponse{}, asyncOrderInvalidHashBatch(), nil
	}

	hashes, rpcErr := parseAsyncOrderHashBatch(req.Hashes)
	if rpcErr != nil {
		return AsyncOrderNewResponse{}, rpcErr, nil
	}

	var resp AsyncOrderNewResponse
	err := s.inDBTx(ctx, func(tx *sql.Tx) error {
		order, err := s.bootstrapAsyncOrderTx(ctx, tx, req.PeerPubkey)
		if err != nil {
			return err
		}
		if order.Status != asyncOrderStatusActive {
			return fmt.Errorf("async order %d is not active", order.OrderID)
		}

		if rpcErr := s.mergeAsyncHashPoolTx(ctx, tx, order, hashes); rpcErr != nil {
			return rpcErr
		}

		snapshot, err := s.asyncOrderSnapshotTx(ctx, tx, order.OrderID)
		if err != nil {
			return err
		}
		resp = snapshot
		return nil
	})
	if err != nil {
		var rpcErr *AsyncOrderError
		if errors.As(err, &rpcErr) {
			return AsyncOrderNewResponse{}, rpcErr, nil
		}
		return AsyncOrderNewResponse{}, nil, err
	}
	return resp, nil, nil
}

func parseAsyncOrderHashBatch(hashes []AsyncOrderNewHashInput) ([]parsedAsyncOrderHash, *AsyncOrderError) {
	parsed := make([]parsedAsyncOrderHash, 0, len(hashes))
	var previous *int64

	for _, entry := range hashes {
		index, err := strconv.ParseInt(strings.TrimSpace(entry.HashIndex), 10, 64)
		if err != nil || index <= 0 {
			return nil, asyncOrderInvalidHashBatch()
		}
		paymentHash := strings.ToLower(strings.TrimSpace(entry.PaymentHash))
		if !isValidPaymentHash(paymentHash) {
			return nil, asyncOrderInvalidHashBatch()
		}
		if previous != nil && index != *previous+1 {
			return nil, asyncOrderInvalidHashBatch()
		}
		previousIndex := index
		previous = &previousIndex
		parsed = append(parsed, parsedAsyncOrderHash{
			HashIndex:   index,
			PaymentHash: paymentHash,
		})
	}

	return parsed, nil
}

func isValidPaymentHash(paymentHash string) bool {
	if len(paymentHash) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(paymentHash)
	return err == nil && len(decoded) == 32
}

func (s *SQLStore) mergeAsyncHashPoolTx(ctx context.Context, tx *sql.Tx, order asyncOrderRow, hashes []parsedAsyncOrderHash) *AsyncOrderError {
	existing, err := s.loadAsyncHashPoolTx(ctx, tx, order.OrderID)
	if err != nil {
		return asyncOrderInternalError(err)
	}

	highestIndex := int64(0)
	hashToIndex := make(map[string]int64, len(existing))
	for index, paymentHash := range existing {
		if index > highestIndex {
			highestIndex = index
		}
		hashToIndex[paymentHash] = index
	}

	currentAcceptedThroughIndex := int64(0)
	if order.AcceptedThroughIndex.Valid {
		currentAcceptedThroughIndex = order.AcceptedThroughIndex.Int64
	}
	if currentAcceptedThroughIndex > highestIndex {
		return asyncOrderInternalError(fmt.Errorf("async order %d accepted_through_index %d exceeds hash pool watermark %d", order.OrderID, currentAcceptedThroughIndex, highestIndex))
	}

	expectedStart := highestIndex + 1
	sawExisting := false
	sawMissing := false
	seenBatchHashes := make(map[string]struct{}, len(hashes))

	for _, entry := range hashes {
		if _, ok := seenBatchHashes[entry.PaymentHash]; ok {
			return asyncOrderDuplicateHashConflict()
		}
		seenBatchHashes[entry.PaymentHash] = struct{}{}

		if existingHash, ok := existing[entry.HashIndex]; ok {
			if existingHash != entry.PaymentHash {
				return asyncOrderDuplicateIndexConflict()
			}
			sawExisting = true
		} else {
			sawMissing = true
		}

		if existingIndex, ok := hashToIndex[entry.PaymentHash]; ok && existingIndex != entry.HashIndex {
			return asyncOrderDuplicateHashConflict()
		}
	}

	missingCount := 0
	for _, entry := range hashes {
		if _, ok := existing[entry.HashIndex]; !ok {
			missingCount++
		}
	}
	if len(existing)+missingCount > asyncHashPoolMaxSize {
		return asyncOrderInvalidHashBatch()
	}

	if sawExisting && sawMissing {
		return asyncOrderInvalidHashBatch()
	}
	if sawExisting {
		if err := s.updateAsyncOrderAcceptedThroughIndexTx(ctx, tx, order.OrderID, highestIndex); err != nil {
			return asyncOrderInternalError(err)
		}
		return nil
	}
	if hashes[0].HashIndex != expectedStart {
		return asyncOrderInvalidHashBatch()
	}

	for _, entry := range hashes {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO async_hash_pool (order_id, hash_index, payment_hash, status)
			VALUES (?, ?, ?, ?)
		`, order.OrderID, entry.HashIndex, entry.PaymentHash, asyncPoolStatusAvailable); err != nil {
			return asyncOrderInternalError(err)
		}
	}

	acceptedThroughIndex := hashes[len(hashes)-1].HashIndex
	if currentAcceptedThroughIndex > acceptedThroughIndex {
		acceptedThroughIndex = currentAcceptedThroughIndex
	}
	if err := s.updateAsyncOrderAcceptedThroughIndexTx(ctx, tx, order.OrderID, acceptedThroughIndex); err != nil {
		return asyncOrderInternalError(err)
	}
	return nil
}

func (s *SQLStore) loadAsyncHashPoolTx(ctx context.Context, tx *sql.Tx, orderID int64) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT hash_index, payment_hash FROM async_hash_pool WHERE order_id = ?`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make(map[int64]string)
	for rows.Next() {
		var hashIndex int64
		var paymentHash string
		if err := rows.Scan(&hashIndex, &paymentHash); err != nil {
			return nil, err
		}
		entries[hashIndex] = strings.ToLower(paymentHash)
	}
	return entries, rows.Err()
}

func (s *SQLStore) asyncOrderSnapshotTx(ctx context.Context, tx *sql.Tx, orderID int64) (AsyncOrderNewResponse, error) {
	acceptedThroughIndex, err := s.loadAsyncOrderAcceptedThroughIndexTx(ctx, tx, orderID)
	if err != nil {
		return AsyncOrderNewResponse{}, err
	}

	var unusedHashes int64
	if !acceptedThroughIndex.Valid {
		query := `
			SELECT
				COALESCE(MAX(hash_index), 0),
				COUNT(CASE WHEN status = ? THEN 1 END)
			FROM async_hash_pool
			WHERE order_id = ?
		`
		if err := tx.QueryRowContext(ctx, query, asyncPoolStatusAvailable, orderID).Scan(&acceptedThroughIndex.Int64, &unusedHashes); err != nil {
			return AsyncOrderNewResponse{}, err
		}
		acceptedThroughIndex.Valid = true
		if err := s.updateAsyncOrderAcceptedThroughIndexTx(ctx, tx, orderID, acceptedThroughIndex.Int64); err != nil {
			return AsyncOrderNewResponse{}, err
		}
	} else {
		query := `SELECT COUNT(CASE WHEN status = ? THEN 1 END) FROM async_hash_pool WHERE order_id = ?`
		if err := tx.QueryRowContext(ctx, query, asyncPoolStatusAvailable, orderID).Scan(&unusedHashes); err != nil {
			return AsyncOrderNewResponse{}, err
		}
	}

	return AsyncOrderNewResponse{
		ProtocolVersion:      asyncOrderProtocolVersion,
		OrderID:              strconv.FormatInt(orderID, 10),
		Status:               asyncOrderStatusActive,
		AcceptedThroughIndex: strconv.FormatInt(acceptedThroughIndex.Int64, 10),
		NextIndexExpected:    strconv.FormatInt(acceptedThroughIndex.Int64+1, 10),
		UnusedHashes:         strconv.FormatInt(unusedHashes, 10),
		RefillBatchSize:      strconv.Itoa(asyncHashPoolMaxSize),
	}, nil
}

func asyncOrderInvalidHashBatch() *AsyncOrderError {
	return &AsyncOrderError{Code: asyncOrderErrorInvalidHashBatch, Message: "invalid_hash_batch"}
}

func asyncOrderDuplicateIndexConflict() *AsyncOrderError {
	return &AsyncOrderError{Code: asyncOrderErrorDuplicateIndexConflict, Message: "duplicate_index_conflict"}
}

func asyncOrderDuplicateHashConflict() *AsyncOrderError {
	return &AsyncOrderError{Code: asyncOrderErrorDuplicateHashConflict, Message: "duplicate_hash_conflict"}
}

func asyncOrderInternalError(err error) *AsyncOrderError {
	return &AsyncOrderError{Code: asyncOrderJSONRPCInternalError, Message: err.Error()}
}

func (s *SQLStore) bootstrapAsyncOrderTx(ctx context.Context, tx *sql.Tx, peerPubkey string) (asyncOrderRow, error) {
	peerPubkey = normalizePeerPubkey(peerPubkey)
	if peerPubkey == "" {
		return asyncOrderRow{}, errors.New("empty peer_pubkey")
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO async_orders (peer_pubkey, status)
		VALUES (?, ?)
	`, peerPubkey, asyncOrderStatusActive); err != nil {
		return asyncOrderRow{}, err
	}

	query := `SELECT order_id, peer_pubkey, status, accepted_through_index, current_invoice_slot, current_hash_index, current_payment_hash, current_invoice_id, created_at, updated_at FROM async_orders WHERE peer_pubkey = ? LIMIT 1`
	row := tx.QueryRowContext(ctx, query, peerPubkey)
	return scanAsyncOrderRow(row)
}

func scanAsyncOrderRow(row rowScanner) (asyncOrderRow, error) {
	var order asyncOrderRow
	if err := row.Scan(&order.OrderID, &order.PeerPubkey, &order.Status, &order.AcceptedThroughIndex, &order.CurrentInvoiceSlot, &order.CurrentHashIndex, &order.CurrentPaymentHash, &order.CurrentInvoiceID, &order.CreatedAt, &order.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return asyncOrderRow{}, errAsyncOrderNotFound
		}
		return asyncOrderRow{}, err
	}
	return order, nil
}

func (s *SQLStore) countAvailableAsyncHashPoolTx(ctx context.Context, tx *sql.Tx, orderID int64) (int64, error) {
	query := `SELECT COUNT(*) FROM async_hash_pool WHERE order_id = ? AND status = ?`
	var count int64
	if err := tx.QueryRowContext(ctx, query, orderID, asyncPoolStatusAvailable).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SQLStore) reserveAsyncHashPoolEntryTx(ctx context.Context, tx *sql.Tx, orderID int64) (asyncHashPoolRow, error) {
	query := `SELECT id, order_id, hash_index, payment_hash, status, created_at, updated_at FROM async_hash_pool WHERE order_id = ? AND status = ? ORDER BY hash_index ASC LIMIT 1`
	row := tx.QueryRowContext(ctx, query, orderID, asyncPoolStatusAvailable)
	var entry asyncHashPoolRow
	if err := row.Scan(&entry.ID, &entry.OrderID, &entry.HashIndex, &entry.PaymentHash, &entry.Status, &entry.CreatedAt, &entry.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return asyncHashPoolRow{}, errAsyncHashPoolEmpty
		}
		return asyncHashPoolRow{}, err
	}

	update := `UPDATE async_hash_pool SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`
	res, err := tx.ExecContext(ctx, update, asyncPoolStatusReserved, entry.ID, asyncPoolStatusAvailable)
	if err != nil {
		return asyncHashPoolRow{}, err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return asyncHashPoolRow{}, errAsyncHashPoolEmpty
	}
	entry.Status = asyncPoolStatusReserved
	return entry, nil
}

func (s *SQLStore) insertAsyncRotatingInvoiceTx(ctx context.Context, tx *sql.Tx, invoice AsyncRotatingInvoice) (int64, error) {
	var assetIDValue any
	if invoice.AssetID != nil {
		assetIDValue = strings.TrimSpace(*invoice.AssetID)
	}
	var assetAmountValue any
	if invoice.AssetAmount != nil {
		assetAmountValue = *invoice.AssetAmount
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO async_rotating_invoices (order_id, invoice_slot, hash_index, payment_hash, asset_amount, asset_id, amount_msat, expires_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, invoice.OrderID, invoice.InvoiceSlot, invoice.HashIndex, invoice.PaymentHash, assetAmountValue, assetIDValue, invoice.AmountMsat, invoice.ExpiresAt, invoice.Status)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *SQLStore) loadAsyncRotatingInvoiceTx(ctx context.Context, tx *sql.Tx, reservationID int64) (asyncRotatingInvoiceRow, error) {
	query := `SELECT id, order_id, invoice_slot, hash_index, payment_hash, asset_amount, asset_id, invoice_string, amount_msat, expires_at, status, created_at, updated_at FROM async_rotating_invoices WHERE id = ? LIMIT 1`
	row := tx.QueryRowContext(ctx, query, reservationID)
	var rec asyncRotatingInvoiceRow
	if err := row.Scan(&rec.ID, &rec.OrderID, &rec.InvoiceSlot, &rec.HashIndex, &rec.PaymentHash, &rec.AssetAmount, &rec.AssetID, &rec.InvoiceString, &rec.AmountMsat, &rec.ExpiresAt, &rec.Status, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return asyncRotatingInvoiceRow{}, errAsyncInvoiceNotFound
		}
		return asyncRotatingInvoiceRow{}, err
	}
	return rec, nil
}

func (s *SQLStore) loadAsyncRotatingInvoiceByPaymentHashTx(ctx context.Context, tx *sql.Tx, paymentHash string) (asyncRotatingInvoiceRow, error) {
	query := `SELECT id, order_id, invoice_slot, hash_index, payment_hash, asset_amount, asset_id, invoice_string, amount_msat, expires_at, status, created_at, updated_at FROM async_rotating_invoices WHERE payment_hash = ? LIMIT 1`
	row := tx.QueryRowContext(ctx, query, paymentHash)
	var rec asyncRotatingInvoiceRow
	if err := row.Scan(&rec.ID, &rec.OrderID, &rec.InvoiceSlot, &rec.HashIndex, &rec.PaymentHash, &rec.AssetAmount, &rec.AssetID, &rec.InvoiceString, &rec.AmountMsat, &rec.ExpiresAt, &rec.Status, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return asyncRotatingInvoiceRow{}, errAsyncInvoiceNotFound
		}
		return asyncRotatingInvoiceRow{}, err
	}
	return rec, nil
}

func (s *SQLStore) finalizeAsyncRotatingInvoiceTx(ctx context.Context, tx *sql.Tx, reservationID int64, invoice string) error {
	query := `UPDATE async_rotating_invoices SET invoice_string = ?, status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	_, err := tx.ExecContext(ctx, query, invoice, asyncInvoiceStatusActive, reservationID)
	return err
}

func (s *SQLStore) MarkAsyncRotatingInvoiceClaimable(ctx context.Context, paymentHash string, amountMsat uint64, assetID *string, assetAmount *uint64) error {
	paymentHash = strings.ToLower(strings.TrimSpace(paymentHash))
	if !isValidPaymentHash(paymentHash) {
		return errors.New("invalid payment_hash")
	}
	if amountMsat == 0 {
		return errAsyncRotatingInvoiceInvalidAmountMsat
	}
	if assetID != nil && strings.TrimSpace(*assetID) == "" {
		return errAsyncRotatingInvoiceInvalidAssetID
	}
	if assetAmount != nil && *assetAmount == 0 {
		return errAsyncRotatingInvoiceInvalidAssetAmount
	}

	return s.inDBTx(ctx, func(tx *sql.Tx) error {
		rec, err := s.loadAsyncRotatingInvoiceByPaymentHashTx(ctx, tx, paymentHash)
		if err != nil {
			return err
		}

		if rec.AmountMsat != amountMsat {
			return errAsyncRotatingInvoiceAmountMsatMismatch
		}
		if rec.AssetID.Valid {
			if assetID == nil || strings.TrimSpace(*assetID) != rec.AssetID.String {
				return errAsyncRotatingInvoiceAssetIDMismatch
			}
		}
		if rec.AssetAmount.Valid {
			if assetAmount == nil || int64(*assetAmount) != rec.AssetAmount.Int64 {
				return errAsyncRotatingInvoiceAssetAmountMismatch
			}
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE async_rotating_invoices
			SET claimable_at = COALESCE(claimable_at, CURRENT_TIMESTAMP),
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`, rec.ID)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return errAsyncInvoiceNotFound
		}
		return nil
	})
}

func (s *SQLStore) markAsyncRotatingInvoiceFailedTx(ctx context.Context, tx *sql.Tx, reservationID int64) error {
	query := `UPDATE async_rotating_invoices SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	_, err := tx.ExecContext(ctx, query, asyncInvoiceStatusFailed, reservationID)
	return err
}

func (s *SQLStore) consumeAsyncHashPoolEntryTx(ctx context.Context, tx *sql.Tx, orderID, hashIndex int64) error {
	query := `UPDATE async_hash_pool SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE order_id = ? AND hash_index = ?`
	_, err := tx.ExecContext(ctx, query, asyncPoolStatusConsumed, orderID, hashIndex)
	return err
}

func (s *SQLStore) releaseAsyncHashPoolEntryTx(ctx context.Context, tx *sql.Tx, orderID, hashIndex int64) error {
	query := `UPDATE async_hash_pool SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE order_id = ? AND hash_index = ?`
	_, err := tx.ExecContext(ctx, query, asyncPoolStatusAvailable, orderID, hashIndex)
	return err
}

func (s *SQLStore) updateAsyncOrderCurrentInvoiceTx(ctx context.Context, tx *sql.Tx, orderID, invoiceID, invoiceSlot, hashIndex int64, paymentHash string) error {
	query := `UPDATE async_orders SET current_invoice_id = ?, current_invoice_slot = ?, current_hash_index = ?, current_payment_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE order_id = ?`
	_, err := tx.ExecContext(ctx, query, invoiceID, invoiceSlot, hashIndex, paymentHash, orderID)
	return err
}

func (s *SQLStore) updateAsyncOrderAcceptedThroughIndexTx(ctx context.Context, tx *sql.Tx, orderID, acceptedThroughIndex int64) error {
	query := `UPDATE async_orders SET accepted_through_index = ?, updated_at = CURRENT_TIMESTAMP WHERE order_id = ?`
	_, err := tx.ExecContext(ctx, query, acceptedThroughIndex, orderID)
	return err
}

func (s *SQLStore) loadAsyncOrderAcceptedThroughIndexTx(ctx context.Context, tx *sql.Tx, orderID int64) (sql.NullInt64, error) {
	query := `SELECT accepted_through_index FROM async_orders WHERE order_id = ? LIMIT 1`
	row := tx.QueryRowContext(ctx, query, orderID)
	var acceptedThroughIndex sql.NullInt64
	if err := row.Scan(&acceptedThroughIndex); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullInt64{}, errAsyncOrderNotFound
		}
		return sql.NullInt64{}, err
	}
	return acceptedThroughIndex, nil
}

func nextInt64FromCurrent(current sql.NullInt64) int64 {
	if current.Valid && current.Int64 > 0 {
		return current.Int64 + 1
	}
	return 1
}
