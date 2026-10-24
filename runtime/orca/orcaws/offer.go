// Package orcaws talks to the Orca runtime over its paired WebSocket, as the Orca CLI does:
// a NaCl box handshake, then sealed JSON requests and replies.
package orcaws

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// OfferVersion is the pairing offer version that this client reads.
const OfferVersion = 2

// maxLink is the longest link that Orca accepts.
const maxLink = 128<<10 + 1024

// Offer is a runtime pairing offer from Orca › Settings. It holds the device token: keep it secret.
type Offer struct {
	V           int    `json:"v"`
	Endpoint    string `json:"endpoint"`
	DeviceToken string `json:"deviceToken"`
	PublicKey   string `json:"publicKeyB64"`
	Scope       string `json:"scope,omitempty"`
}

// ParseLink reads an orca://pair link or a bare pairing code.
func ParseLink(link string) (Offer, error) {
	link = strings.TrimSpace(link)
	if link == "" || len(link) > maxLink {
		return Offer{}, errors.New("The pairing link is empty or too long.")
	}
	code := link
	if strings.HasPrefix(strings.ToLower(link), "orca://") {
		u, err := url.Parse(link)
		if err != nil || u.Host != "pair" || (u.Path != "" && u.Path != "/") {
			return Offer{}, errors.New("The pairing link must start with orca://pair.")
		}
		if code = u.Query().Get("code"); code == "" {
			code = u.Fragment
		}
	}
	return ParseCode(code)
}

// ParseCode reads the base64url code of a pairing link.
func ParseCode(code string) (Offer, error) {
	code = strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(code), "=")
	b, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		return Offer{}, errors.New("The pairing code is not valid.")
	}
	var o struct {
		Offer
		Relay json.RawMessage `json:"relay"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return Offer{}, errors.New("The pairing code is not valid.")
	}
	switch {
	case o.V != OfferVersion:
		return Offer{}, fmt.Errorf("The pairing link has version %d. The daemon reads version %d.", o.V, OfferVersion)
	case o.Scope == "mobile":
		return Offer{}, errors.New("This is a mobile pairing link. In Orca, open Settings and make a runtime pairing link.")
	case len(o.Relay) > 0 && string(o.Relay) != "null":
		return Offer{}, errors.New("The daemon cannot pair through the Orca relay. Make a link for this computer only.")
	case o.Endpoint == "" || o.DeviceToken == "" || o.PublicKey == "":
		return Offer{}, errors.New("The pairing link has no endpoint, token or key.")
	}
	if _, err := o.hostKey(); err != nil {
		return Offer{}, err
	}
	if _, err := o.endpoint(); err != nil {
		return Offer{}, err
	}
	return o.Offer, nil
}

// Code is the offer as a base64url code, for the keychain.
func (o Offer) Code() string {
	b, _ := json.Marshal(o)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (o Offer) hostKey() (*[32]byte, error) {
	b, err := base64.StdEncoding.DecodeString(o.PublicKey)
	if err != nil || len(b) != 32 {
		return nil, errors.New("The pairing link has a bad Orca key.")
	}
	var k [32]byte
	copy(k[:], b)
	return &k, nil
}

// endpoint gives the WebSocket URL. A wildcard host is this machine.
func (o Offer) endpoint() (string, error) {
	u, err := url.Parse(o.Endpoint)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return "", fmt.Errorf("The pairing link has a bad endpoint %q.", o.Endpoint)
	}
	switch u.Hostname() {
	case "0.0.0.0", "::":
		u.Host = strings.Replace(u.Host, u.Hostname(), "127.0.0.1", 1)
	}
	return u.String(), nil
}
