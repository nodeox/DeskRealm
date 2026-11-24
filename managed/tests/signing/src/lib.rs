#![cfg(test)]

#[allow(dead_code)]
#[path = "../../../native/deskrealm_protocol.rs"]
mod deskrealm_protocol;

mod tests {
    use crate::deskrealm_protocol::*;
    use serde_json::Value;
    use sodiumoxide::crypto::sign;

    fn hex(s: &str) -> Vec<u8> {
        assert_eq!(s.len() % 2, 0);
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
            .collect()
    }

    #[test]
    fn shared_signing_vectors() {
        sodiumoxide::init().expect("libsodium initialization");
        let fixtures: Value = serde_json::from_str(include_str!("../../vectors.json")).unwrap();
        assert_eq!(fixtures["version"], 1);
        let mut verified_kinds = Vec::new();
        let mut rejected = 0;
        for vector in fixtures["vectors"].as_array().unwrap() {
            let name = vector["name"].as_str().unwrap();
            let kind: u8 = vector["kind"].as_u64().unwrap().try_into().unwrap();
            let fields: Vec<_> = vector["fields_hex"]
                .as_array()
                .unwrap()
                .iter()
                .map(|v| hex(v.as_str().unwrap()))
                .collect();
            let fields: Vec<&[u8]> = fields.iter().map(Vec::as_slice).collect();
            let encoded = encode(kind, &fields);
            if vector["reject"] == true {
                assert!(encoded.is_none(), "{name}: invalid frame accepted");
                rejected += 1;
                continue;
            }
            let mut frame = encoded.expect(name);
            assert_eq!(frame, hex(vector["frame_hex"].as_str().unwrap()), "{name}");
            let seed = sign::Seed::from_slice(&hex(vector["seed_hex"].as_str().unwrap())).unwrap();
            let (public, secret) = sign::keypair_from_seed(&seed);
            assert_eq!(
                public.as_ref(),
                hex(vector["public_key_hex"].as_str().unwrap()),
                "{name}"
            );
            let signature =
                sign::Signature::from_bytes(&hex(vector["signature_hex"].as_str().unwrap()))
                    .unwrap();
            assert_eq!(sign::sign_detached(&frame, &secret), signature, "{name}");
            assert!(sign::verify_detached(&signature, &frame, &public), "{name}");
            frame[7] ^= 0x01;
            assert!(
                !sign::verify_detached(&signature, &frame, &public),
                "{name}: changed kind"
            );
            verified_kinds.push(kind);
        }
        verified_kinds.sort_unstable();
        verified_kinds.dedup();
        assert_eq!(verified_kinds, [1, 2, 3, 4, 5, 6, 7, 8]);
        assert_eq!(rejected, 20);
    }

    #[test]
    fn managed_peer_proofs_are_channel_and_role_bound() {
        sodiumoxide::init().unwrap();
        let fixtures: Value = serde_json::from_str(include_str!("../../vectors.json")).unwrap();
        let vector = |name| {
            fixtures["vectors"]
                .as_array()
                .unwrap()
                .iter()
                .find(|v| v["name"] == name)
                .unwrap()
        };
        let bytes = |v: &Value, field: &str| hex(v[field].as_str().unwrap());
        let key = |v: &Value| {
            let seed = sign::Seed::from_slice(&bytes(v, "seed_hex")).unwrap();
            sign::keypair_from_seed(&seed)
        };
        let grant_vector = vector("center-session-grant");
        let frame = bytes(grant_vector, "frame_hex");
        let signature = bytes(grant_vector, "signature_hex");
        let (center, _) = key(grant_vector);
        let (controller, controller_secret) = key(vector("controller-direct-proof"));
        let (target, target_secret) = key(vector("target-relay-proof"));
        let field = |index: usize| hex(grant_vector["fields_hex"][index].as_str().unwrap());
        let issued = u64::from_be_bytes(field(13).try_into().unwrap());
        let expires = u64::from_be_bytes(field(14).try_into().unwrap());
        let verify = |frame, signature, kid, key, now| {
            VerifiedGrant::verify(frame, signature, kid, key, now)
        };
        let grant = verify(&frame, &signature, "test-key-01", &center, issued).unwrap();
        assert!(verify(&frame, &signature, "unknown", &center, issued).is_none());
        assert!(verify(&frame, &signature, "test-key-01", &target, issued).is_none());
        assert!(verify(&frame, &signature, "test-key-01", &center, issued - 1).is_none());
        assert!(verify(&frame, &signature, "test-key-01", &center, expires).is_none());
        assert!(verify(
            &frame[..frame.len() - 1],
            &signature,
            "test-key-01",
            &center,
            issued
        )
        .is_none());
        assert!(verify(&frame, &signature[..63], "test-key-01", &center, issued).is_none());
        assert!(grant.attended());
        assert_eq!(grant.allowed_mask(), 7);
        assert_eq!(grant.policy_version(), 9);

        let binding = channel_binding(&[3; 32], &[1; 32], &[2; 32], 1, 1).unwrap();
        assert_eq!(binding.as_bytes().as_slice(), hex("604fc00cd205f403b26d6f3dd38abc39a6a7cc8f019989df88409dd3a579f694"));
        assert!(channel_binding(&[3; 32], &[1; 32], &[2; 32], 0, 0).is_none());
        assert!(channel_binding(&[3; 32], &[1; 32], &[1; 32], 1, 1).is_none());
        assert!(channel_binding(&[3; 32], &[1; 31], &[2; 32], 1, 1).is_none());
        let conn = [3; 16];
        let bind = |id, channel, left, right, path| grant.bind(id, channel, left, right, path);
        let proof = bind(conn, binding, [1; 32], [2; 32], Path::Direct).unwrap();
        assert!(bind(conn, binding, [1; 32], [1; 32], Path::Direct).is_none());
        let controller_sig =
            sign::sign_detached(&proof.proof_frame(PeerRole::Controller), &controller_secret)
                .to_bytes();
        let target_sig =
            sign::sign_detached(&proof.proof_frame(PeerRole::Target), &target_secret).to_bytes();
        assert!(proof.verify_peer(PeerRole::Controller, &controller, &controller_sig));
        assert!(proof.verify_peer(PeerRole::Target, &target, &target_sig));
        assert!(!proof.verify_peer(PeerRole::Controller, &target, &target_sig));
        assert!(!proof.verify_peer(PeerRole::Target, &controller, &target_sig));

        let other = channel_binding(&[4; 32], &[1; 32], &[2; 32], 1, 1).unwrap();
        let attempts = [
            bind([0; 16], binding, [1; 32], [2; 32], Path::Direct),
            bind(conn, other, [1; 32], [2; 32], Path::Direct),
            bind(conn, binding, [9; 32], [2; 32], Path::Direct),
            bind(conn, binding, [1; 32], [2; 32], Path::Relay),
        ];
        assert!(attempts
            .into_iter()
            .all(|attempt| !attempt.unwrap().verify_peer(
                PeerRole::Controller,
                &controller,
                &controller_sig
            )));
        let consent =
            sign::sign_detached(&proof.consent_frame(1).unwrap(), &target_secret).to_bytes();
        assert!(proof.verifies_consent(&target, 1, &consent));
        assert!(!proof.verifies_consent(&target, 3, &consent));
        assert!(!proof.verifies_consent(&controller, 1, &consent));
        assert!(proof.consent_frame(8).is_none());
    }
}
