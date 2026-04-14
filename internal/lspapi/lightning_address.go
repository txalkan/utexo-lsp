package lspapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (a *API) lightningAddressDomain() (string, error) {
	parsed, err := parseLightningAddressDomainURL(a.cfg.LightningAddressDomainURL)
	if err != nil {
		return "", err
	}
	return parsed.Host, nil
}

func (a *API) lightningAddressCallbackDomainURL() (string, error) {
	parsed, err := parseLightningAddressDomainURL(a.cfg.LightningAddressDomainURL)
	if err != nil {
		return "", err
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func (a *API) lightningAddressCallbackURL(account LightningAddressAccount) (string, error) {
	baseURL, err := a.lightningAddressCallbackDomainURL()
	if err != nil {
		return "", err
	}

	handle := account.Username
	if handle == "" {
		return "", errors.New("empty lightning address handle")
	}
	return baseURL + "/pay/callback/" + url.PathEscape(handle), nil
}

func (a *API) lightningAddressMetadata(account LightningAddressAccount) (string, string, error) {
	domain, err := a.lightningAddressDomain()
	if err != nil {
		return "", "", err
	}

	handle := account.Username
	if handle == "" {
		return "", "", errors.New("empty lightning address handle")
	}

	address := handle + "@" + domain
	shortDesc := strings.TrimSpace(a.cfg.LightningAddressShortDescription)
	if shortDesc == "" {
		shortDesc = address
	}

	metadataEntries := make([][2]string, 0, 3)
	metadataEntries = append(metadataEntries, [2]string{"text/identifier", address})
	metadataEntries = append(metadataEntries, [2]string{"text/plain", shortDesc})

	metadata, err := json.Marshal(metadataEntries)
	if err != nil {
		return "", "", err
	}

	return address, string(metadata), nil
}

func (a *API) handleLightningAddressDiscovery(w http.ResponseWriter, r *http.Request) {
	account, ok, err := a.lightningAddressAccount(r.Context(), r.PathValue("username"))
	if !ok {
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"status": "ERROR",
				"reason": fmt.Sprintf("failed to resolve lightning address account: %v", err),
			})
			return
		}
		http.NotFound(w, r)
		return
	}

	_, metadata, err := a.lightningAddressMetadata(account)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "ERROR",
			"reason": fmt.Sprintf("failed to build lightning address metadata: %v", err),
		})
		return
	}
	callbackURL, err := a.lightningAddressCallbackURL(account)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "ERROR",
			"reason": fmt.Sprintf("failed to build lightning address callback url: %v", err),
		})
		return
	}
	writeJSON(w, http.StatusOK, LightningAddressDiscoveryResponse{
		Callback:    callbackURL,
		MaxSendable: a.cfg.LightningAddressMaxSendableMsat,
		MinSendable: a.cfg.LightningAddressMinSendableMsat,
		Metadata:    metadata,
		Tag:         "payRequest",
	})
}

func (a *API) handleLightningAddressCallback(w http.ResponseWriter, r *http.Request) {
	account, ok, err := a.lightningAddressAccount(r.Context(), r.PathValue("username"))
	if !ok {
		if err != nil {
			writeLightningAddressError(w, http.StatusInternalServerError, fmt.Sprintf("failed to resolve lightning address account: %v", err))
			return
		}
		http.NotFound(w, r)
		return
	}

	amountStr := strings.TrimSpace(r.URL.Query().Get("amount"))
	if amountStr == "" {
		writeLightningAddressError(w, http.StatusBadRequest, "missing amount")
		return
	}

	amountMsat, err := strconv.ParseUint(amountStr, 10, 64)
	if err != nil {
		writeLightningAddressError(w, http.StatusBadRequest, fmt.Sprintf("cannot parse amount: %v", err))
		return
	}
	if amountMsat < a.cfg.LightningAddressMinSendableMsat || amountMsat > a.cfg.LightningAddressMaxSendableMsat {
		writeLightningAddressError(w, http.StatusBadRequest, "amount is out of acceptable range")
		return
	}

	_, metadata, err := a.lightningAddressMetadata(account)
	if err != nil {
		writeLightningAddressError(w, http.StatusInternalServerError, fmt.Sprintf("failed to build lightning address metadata: %v", err))
		return
	}
	invoice, err := a.createLightningAddressInvoice(r.Context(), amountMsat, metadata)
	if err != nil {
		writeLightningAddressError(w, http.StatusInternalServerError, fmt.Sprintf("error constructing invoice: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, LightningAddressCallbackResponse{
		PR:     invoice,
		Routes: []string{},
	})
}

func (a *API) createLightningAddressInvoice(ctx context.Context, amountMsat uint64, metadata string) (string, error) {
	payload := LNInvoiceInput{
		AmtMsat:   &amountMsat,
		ExpirySec: uint32(a.cfg.LightningAddressInvoiceExpiry.Seconds()),
	}
	if metadata != "" {
		sum := sha256.Sum256([]byte(metadata))
		hash := hex.EncodeToString(sum[:])
		payload.DescriptionHash = &hash
	}

	var resp struct {
		Invoice string `json:"invoice"`
	}
	if err := a.lspClient.DoJSON(ctx, http.MethodPost, a.cfg.LNInvoicePath, payload, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Invoice) == "" {
		return "", errors.New("empty lsp lightning invoice")
	}
	return resp.Invoice, nil
}

func writeLightningAddressError(w http.ResponseWriter, code int, reason string) {
	writeJSON(w, code, map[string]string{
		"status": "ERROR",
		"reason": reason,
	})
}
