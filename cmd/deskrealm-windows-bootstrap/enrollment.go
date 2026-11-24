package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"deskrealm/internal/managedproto"
	"deskrealm/internal/windowsdelivery"
)

type enrollmentRequest struct {
	Credential string `json:"credential"`
	Group      string `json:"group_id"`
	Platform   string `json:"platform"`
	RustDeskID string `json:"rustdesk_id"`
	PublicKey  string `json:"public_key"`
	Attempt    string `json:"attempt_id,omitempty"`
	Signature  string `json:"signature,omitempty"`
}

type challenge struct {
	ID    string `json:"attempt_id"`
	Nonce string `json:"nonce"`
}

var errEnrollmentUnauthorized = errors.New("center rejected enrollment")

func bootstrap(ctx context.Context, client *http.Client, center windowsdelivery.Center, state *os.Root,
	platform, rustdeskID string, key ed25519.PrivateKey) (registration, error) {
	saved, err := readRegistration(state)
	if !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = cleanDeployment(state)
		}
		return saved, err
	}
	raw, err := readDocument(state, deploymentName)
	if err != nil {
		return registration{}, err
	}
	var deployment struct {
		Credential string `json:"credential"`
		Group      string `json:"group_id"`
		Platform   string `json:"platform"`
		State      string `json:"state"`
	}
	if json.Unmarshal(raw, &deployment) != nil || !validNonce(deployment.Credential) ||
		!uuidPattern.MatchString(deployment.Group) || deployment.Platform != platform || deployment.State != "issued" {
		return registration{}, errors.New("invalid deployment receipt for this platform")
	}
	var signingKey struct {
		ID     string `json:"kid"`
		Public string `json:"public_key"`
	}
	if err := centerRequest(ctx, client, center.Origin+"/v1/managed/signing-key", nil, &signingKey); err != nil {
		return registration{}, err
	}
	if signingKey.ID != center.SigningKeyID || signingKey.Public != center.SigningKey {
		return registration{}, errors.New("center signing identity differs from the installed profile")
	}
	in := enrollmentRequest{Credential: deployment.Credential, Group: deployment.Group, Platform: platform,
		RustDeskID: rustdeskID, PublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}
	sum := sha256.Sum256([]byte(deployment.Credential))
	attemptFile := "challenge-" + hex.EncodeToString(sum[:]) + ".json"
	registered, err := register(ctx, client, center.Origin, state, attemptFile, in, key)
	if err != nil {
		return registration{}, err
	}
	raw, err = json.Marshal(registered)
	if err != nil {
		return registration{}, err
	}
	if err := saveDocument(state, receiptName, raw); err != nil && !errors.Is(err, os.ErrExist) {
		return registration{}, err
	}
	if persisted, err := readRegistration(state); err != nil || persisted != registered {
		return registration{}, errors.New("local registration receipt differs from the center response")
	}
	if err := cleanDeployment(state); err != nil {
		return registration{}, err
	}
	_ = state.Remove(attemptFile)
	return registered, nil
}

func cleanDeployment(state *os.Root) error {
	if err := state.Remove(deploymentName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readRegistration(state *os.Root) (registration, error) {
	var result registration
	raw, err := readDocument(state, receiptName)
	if err == nil && (json.Unmarshal(raw, &result) != nil || !result.valid()) {
		err = errors.New("invalid local registration receipt")
	}
	return result, err
}

func register(ctx context.Context, client *http.Client, origin string, state *os.Root, attemptFile string,
	in enrollmentRequest, key ed25519.PrivateKey) (registration, error) {
	if raw, err := readDocument(state, attemptFile); err == nil {
		var saved challenge
		if json.Unmarshal(raw, &saved) != nil || !saved.valid() {
			return registration{}, errors.New("invalid saved enrollment challenge")
		}
		result, err := complete(ctx, client, origin, in, saved, key)
		if !errors.Is(err, errEnrollmentUnauthorized) {
			return result, err
		}
		if err := state.Remove(attemptFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return registration{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return registration{}, err
	}
	var next challenge
	if err := centerRequest(ctx, client, origin+"/v1/enrollment/challenge", in, &next); err != nil {
		return registration{}, err
	}
	if !next.valid() {
		return registration{}, errors.New("invalid enrollment challenge from center")
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return registration{}, err
	}
	if err := saveDocument(state, attemptFile, raw); err != nil && !errors.Is(err, os.ErrExist) {
		return registration{}, err
	}
	return complete(ctx, client, origin, in, next, key)
}

func (c challenge) valid() bool {
	return uuidPattern.MatchString(c.ID) && validNonce(c.Nonce)
}

func validNonce(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}

func complete(ctx context.Context, client *http.Client, origin string, in enrollmentRequest, proof challenge,
	key ed25519.PrivateKey) (registration, error) {
	in.Attempt = proof.ID
	frame := managedproto.DeviceEnrollmentProof(proof.ID, proof.Nonce, in.Group, in.Platform, in.RustDeskID, in.PublicKey)
	in.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, frame))
	var result registration
	if err := centerRequest(ctx, client, origin+"/v1/enrollment/complete", in, &result); err != nil {
		return registration{}, err
	}
	if !result.valid() {
		return registration{}, errors.New("invalid registration receipt from center")
	}
	return result, nil
}

func centerRequest(ctx context.Context, client *http.Client, url string, in any, out any) error {
	method := http.MethodGet
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		method, body = http.MethodPost, bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("unable to contact DeskRealm center")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errEnrollmentUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("center returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentSize+1))
	if err != nil || len(raw) > maxDocumentSize || json.Unmarshal(raw, out) != nil {
		return errors.New("invalid response from DeskRealm center")
	}
	return nil
}
