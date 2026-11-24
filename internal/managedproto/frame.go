// Package managedproto defines the canonical signed frames for managed sessions.
package managedproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

type Kind byte

const (
	DeviceChallenge  Kind = 1
	UserBinding      Kind = 2
	SessionGrant     Kind = 3
	ConnectionProof  Kind = 4
	PolicyReceipt    Kind = 5
	SessionConsent   Kind = 6
	RelayRoundJoin   Kind = 7
	RelayProbe       Kind = 8
	RelayProbePermit Kind = 9
	RelayProbeHolder Kind = 10
	RelaySelection   Kind = 11
	RelayTicket      Kind = 12
	RelayTicketProof Kind = 13
	RelayPairReceipt Kind = 14
)

const prefix = "DR-M2\x00\x01"

var ErrFrame = errors.New("invalid managed protocol frame")

// Widths are one byte or fixed-length scalars, UUIDs and digests. Zero marks
// an ASCII identifier, whose field-specific limit is applied below.
const (
	uuid   = 16
	digest = 32
	word   = 8
)

var fieldWidths = map[Kind][]int{
	DeviceChallenge:  {uuid, uuid, uuid, digest, word, word, 2},
	UserBinding:      {uuid, uuid, uuid, uuid, digest, digest, word, word},
	SessionGrant:     {0, uuid, uuid, uuid, uuid, uuid, uuid, digest, digest, word, 1, 1, 0, word, word},
	ConnectionProof:  {1, digest, uuid, uuid, digest, digest, digest, 1},
	PolicyReceipt:    {uuid, uuid, uuid, word, digest, word, 1, 1},
	SessionConsent:   {digest, uuid, uuid, digest, 1},
	RelayRoundJoin:   {1, uuid, uuid, uuid},
	RelayProbe:       {1, uuid, uuid, uuid, uuid, uuid, word, digest, 15},
	RelayProbePermit: {1, uuid, uuid, uuid, uuid, uuid, word, digest, digest, word, word},
	RelayProbeHolder: {digest, 1, digest},
	RelaySelection:   {1, uuid, uuid, uuid, uuid, uuid},
	RelayTicket:      {1, uuid, uuid, uuid, uuid, uuid, word, digest, digest, word, word},
	RelayTicketProof: {digest, digest},
	RelayPairReceipt: {uuid, uuid, uuid, uuid, uuid, word},
}

// Encode produces the exact signed bytes. Frame validation checks the wire
// shape only. Callers must still validate identities, policy and freshness.
func Encode(kind Kind, fields ...[]byte) ([]byte, error) {
	widths, defined := fieldWidths[kind]
	if !defined || len(widths) != len(fields) {
		return nil, ErrFrame
	}
	for index, field := range fields {
		if widths[index] == 0 {
			limit := 32
			if index == 12 {
				limit = 64
			}
			if !validIdentifier(field, limit) {
				return nil, fmt.Errorf("%w: field %d", ErrFrame, index)
			}
		} else if len(field) != widths[index] {
			return nil, fmt.Errorf("%w: field %d", ErrFrame, index)
		}
	}
	if !validValues(kind, fields) {
		return nil, ErrFrame
	}
	frame := append([]byte(prefix), byte(kind))
	for _, field := range fields {
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(field)))
		frame = append(frame, field...)
	}
	return frame, nil
}

func validIdentifier(field []byte, limit int) bool {
	if len(field) == 0 || len(field) > limit {
		return false
	}
	for i, b := range field {
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || (i > 0 && (b == '-' || b == '_')) {
			continue
		}
		return false
	}
	return true
}

func positive(value []byte) bool {
	n := binary.BigEndian.Uint64(value)
	return n > 0 && n <= uint64(1<<63-1)
}

func validValues(kind Kind, f [][]byte) bool {
	switch kind {
	case DeviceChallenge:
		return positive(f[4]) && positive(f[5]) && binary.BigEndian.Uint16(f[6]) > 0
	case UserBinding:
		return positive(f[6]) && positive(f[7])
	case SessionGrant:
		issued, expires := binary.BigEndian.Uint64(f[13]), binary.BigEndian.Uint64(f[14])
		return !bytes.Equal(f[5], f[6]) && positive(f[9]) && f[10][0] > 0 && f[10][0]&^0x1f == 0 &&
			(f[11][0] == 1 || f[11][0] == 2) && positive(f[13]) && positive(f[14]) && expires > issued && expires-issued <= 60_000
	case ConnectionProof:
		return (f[0][0] == 1 || f[0][0] == 2) && (f[7][0] == 1 || f[7][0] == 2) && !bytes.Equal(f[5], f[6])
	case PolicyReceipt:
		return positive(f[3]) && positive(f[5]) &&
			((f[6][0] == 1 && f[7][0] == 0) || (f[6][0] == 2 && f[7][0] > 0 && f[7][0] <= 7))
	case SessionConsent:
		return f[4][0] > 0 && f[4][0] <= 31
	case RelayRoundJoin:
		return f[0][0] == 1 || f[0][0] == 2
	case RelayProbe:
		return (f[0][0] == 1 || f[0][0] == 2) && positive(f[6]) && validRelayProbeSamples(f[8])
	case RelayProbePermit, RelayTicket:
		start, expires := binary.BigEndian.Uint64(f[9]), binary.BigEndian.Uint64(f[10])
		window := uint64(2_000)
		if kind == RelayTicket {
			window = 10_000
		}
		return (f[0][0] == 1 || f[0][0] == 2) && positive(f[6]) && positive(f[9]) && positive(f[10]) &&
			expires > start && expires-start <= window
	case RelayProbeHolder:
		return f[1][0] < 3
	case RelaySelection:
		return f[0][0] == 1 || f[0][0] == 2
	case RelayTicketProof:
		return true
	case RelayPairReceipt:
		return positive(f[5])
	default:
		return false
	}
}

func validRelayProbeSamples(samples []byte) bool {
	for offset := 0; offset < len(samples); offset += 5 {
		rtt := binary.BigEndian.Uint32(samples[offset+1 : offset+5])
		switch {
		case samples[offset] == 1 && rtt <= 2_000_000:
		case samples[offset] >= 2 && samples[offset] <= 5 && rtt == 0:
		default:
			return false
		}
	}
	return true
}
