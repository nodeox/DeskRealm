package managedproto

import "strings"

// DeviceEnrollmentProof encodes a challenge. The protocol restricts each field to an alphabet without newlines.
func DeviceEnrollmentProof(attempt, nonce, group, platform, rustdeskID, publicKey string) []byte {
	return []byte(strings.Join([]string{"deskrealm/device-enrollment/v1", attempt, nonce, group, platform, rustdeskID, publicKey}, "\n"))
}
