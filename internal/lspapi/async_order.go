package lspapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const asyncHashPoolSeedBatchSize = 200

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

type asyncOrderRow struct {
	OrderID            int64
	PeerPubkey         string
	Status             string
	CurrentInvoiceSlot sql.NullInt64
	CurrentHashIndex   sql.NullInt64
	CurrentPaymentHash sql.NullString
	CurrentInvoiceID   sql.NullInt64
	CreatedAt          time.Time
	UpdatedAt          time.Time
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
	InvoiceString sql.NullString
	AmountMsat    uint64
	ExpiresAt     time.Time
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
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

func (s *SQLStore) ReserveLightningAddressInvoiceSlot(ctx context.Context, account LightningAddressAccount, amountMsat uint64, expiry time.Duration) (AsyncRotatingInvoice, error) {
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
		nextHashIndex := nextInt64FromCurrent(order.CurrentHashIndex)

		available, err := s.countAvailableAsyncHashPoolTx(ctx, tx, order.OrderID)
		if err != nil {
			return err
		}
		if available == 0 {
			if err := s.seedAsyncHashPoolTx(ctx, tx, order.OrderID, nextHashIndex); err != nil {
				return err
			}
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

	query := `SELECT order_id, peer_pubkey, status, current_invoice_slot, current_hash_index, current_payment_hash, current_invoice_id, created_at, updated_at FROM async_orders WHERE peer_pubkey = ? LIMIT 1`
	row := tx.QueryRowContext(ctx, query, peerPubkey)
	return scanAsyncOrderRow(row)
}

func scanAsyncOrderRow(row rowScanner) (asyncOrderRow, error) {
	var order asyncOrderRow
	if err := row.Scan(&order.OrderID, &order.PeerPubkey, &order.Status, &order.CurrentInvoiceSlot, &order.CurrentHashIndex, &order.CurrentPaymentHash, &order.CurrentInvoiceID, &order.CreatedAt, &order.UpdatedAt); err != nil {
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

func (s *SQLStore) seedAsyncHashPoolTx(ctx context.Context, tx *sql.Tx, orderID, start int64) error {
	// TODO: replace local hash minting with recipient-client supplied hashes once async_order.sync_hashes exists.
	for i := int64(0); i < asyncHashPoolSeedBatchSize; i++ {
		paymentHash, err := mintAsyncPaymentHashHex()
		if err != nil {
			return err
		}
		hashIndex := start + i
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO async_hash_pool (order_id, hash_index, payment_hash, status)
			VALUES (?, ?, ?, ?)
		`, orderID, hashIndex, paymentHash, asyncPoolStatusAvailable); err != nil {
			return err
		}
	}

	return nil
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
	res, err := tx.ExecContext(ctx, `
		INSERT INTO async_rotating_invoices (order_id, invoice_slot, hash_index, payment_hash, amount_msat, expires_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, invoice.OrderID, invoice.InvoiceSlot, invoice.HashIndex, invoice.PaymentHash, invoice.AmountMsat, invoice.ExpiresAt, invoice.Status)
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
	query := `SELECT id, order_id, invoice_slot, hash_index, payment_hash, invoice_string, amount_msat, expires_at, status, created_at, updated_at FROM async_rotating_invoices WHERE id = ? LIMIT 1`
	row := tx.QueryRowContext(ctx, query, reservationID)
	var rec asyncRotatingInvoiceRow
	if err := row.Scan(&rec.ID, &rec.OrderID, &rec.InvoiceSlot, &rec.HashIndex, &rec.PaymentHash, &rec.InvoiceString, &rec.AmountMsat, &rec.ExpiresAt, &rec.Status, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
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

func nextInt64FromCurrent(current sql.NullInt64) int64 {
	if current.Valid && current.Int64 > 0 {
		return current.Int64 + 1
	}
	return 1
}

func mintAsyncPaymentHashHex() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
