package lspapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	haikunator "github.com/txalkan/go-haikunator"
)

const lightningAddressAccountRetryLimit = 128

func normalizeLightningAddressHandle(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	return raw
}

func normalizePeerPubkey(peerPubkey string) string {
	return strings.ToLower(strings.TrimSpace(peerOnly(peerPubkey)))
}

func mintLightningAddressHandle() string {
	return strings.ToLower(haikunator.New().Haikunate())
}

func (a *API) ensureLightningAddressAccount(ctx context.Context, peerPubkey string) (LightningAddressAccount, error) {
	peerPubkey = normalizePeerPubkey(peerPubkey)
	if peerPubkey == "" {
		return LightningAddressAccount{}, errors.New("empty peer_pubkey")
	}

	if a.db == nil {
		return LightningAddressAccount{}, errors.New("lightning address database is not configured")
	}

	existing, err := a.db.GetLightningAddressAccountByPeerPubkey(ctx, peerPubkey)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, errLightningAddressAccountNotFound) {
		return LightningAddressAccount{}, err
	}

	for range [lightningAddressAccountRetryLimit]struct{}{} {
		handle := mintLightningAddressHandle()
		account := LightningAddressAccount{
			PeerPubkey: peerPubkey,
			Username:   handle,
		}

		inserted, err := a.db.InsertLightningAddressAccount(ctx, account)
		if err != nil {
			return LightningAddressAccount{}, err
		}

		if inserted {
			return a.db.GetLightningAddressAccountByPeerPubkey(ctx, peerPubkey)
		}

		existing, err := a.db.GetLightningAddressAccountByPeerPubkey(ctx, peerPubkey)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, errLightningAddressAccountNotFound) {
			return LightningAddressAccount{}, err
		}
	}

	return LightningAddressAccount{}, fmt.Errorf("unable to allocate lightning address for peer_pubkey %s", peerPubkey)
}

func (a *API) lightningAddressAccount(ctx context.Context, rawHandle string) (LightningAddressAccount, bool, error) {
	handle := normalizeLightningAddressHandle(rawHandle)
	if handle == "" {
		return LightningAddressAccount{}, false, nil
	}

	if a.db == nil {
		return LightningAddressAccount{}, false, errors.New("lightning address database is not configured")
	}

	account, err := a.db.GetLightningAddressAccountByUsername(ctx, handle)
	switch {
	case err == nil:
		return account, true, nil
	case errors.Is(err, errLightningAddressAccountNotFound):
		return LightningAddressAccount{}, false, nil
	default:
		return LightningAddressAccount{}, false, err
	}
}
