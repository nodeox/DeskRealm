#![cfg(test)]

const PREFIX: &[u8] = b"DR-M2\0\x01";

fn identifier(field: &[u8], limit: usize) -> bool {
    !field.is_empty()
        && field.len() <= limit
        && field.iter().enumerate().all(|(i, b)| {
            b.is_ascii_lowercase() || b.is_ascii_digit() || (i > 0 && matches!(b, b'-' | b'_'))
        })
}

fn number(field: &[u8]) -> u64 {
    u64::from_be_bytes(field.try_into().expect("validated integer width"))
}

fn positive(field: &[u8]) -> bool {
    (1..=i64::MAX as u64).contains(&number(field))
}

fn valid_values(kind: u8, f: &[Vec<u8>]) -> bool {
    match kind {
        1 => positive(&f[4]) && positive(&f[5]) && f[6] != [0, 0],
        2 => positive(&f[6]) && positive(&f[7]),
        3 => {
            let (issued, expires) = (number(&f[13]), number(&f[14]));
            f[5] != f[6]
                && positive(&f[9])
                && (1..=0x1f).contains(&f[10][0])
                && (1..=2).contains(&f[11][0])
                && positive(&f[13])
                && positive(&f[14])
                && expires > issued
                && expires - issued <= 60_000
        }
        4 => (1..=2).contains(&f[0][0]) && (1..=2).contains(&f[7][0]) && f[5] != f[6],
        5 => {
            positive(&f[3])
                && positive(&f[5])
                && ((f[6][0] == 1 && f[7][0] == 0)
                    || (f[6][0] == 2 && (1..=7).contains(&f[7][0])))
        }
        6 => (1..=0x1f).contains(&f[4][0]),
        _ => false,
    }
}

fn encode(kind: u8, fields: &[Vec<u8>]) -> Option<Vec<u8>> {
    let widths: &[usize] = match kind {
        1 => &[16, 16, 16, 32, 8, 8, 2],
        2 => &[16, 16, 16, 16, 32, 32, 8, 8],
        3 => &[0, 16, 16, 16, 16, 16, 16, 32, 32, 8, 1, 1, 0, 8, 8],
        4 => &[1, 32, 16, 16, 32, 32, 32, 1],
        5 => &[16, 16, 16, 8, 32, 8, 1, 1],
        6 => &[32, 16, 16, 32, 1],
        _ => return None,
    };
    if fields.len() != widths.len()
        || fields
            .iter()
            .zip(widths)
            .enumerate()
            .any(|(index, (field, width))| {
                if *width == 0 {
                    !identifier(field, if index == 12 { 64 } else { 32 })
                } else {
                    field.len() != *width
                }
            })
        || !valid_values(kind, fields)
    {
        return None;
    }
    let mut frame = PREFIX.to_vec();
    frame.push(kind);
    for field in fields {
        frame.extend_from_slice(&(field.len() as u16).to_be_bytes());
        frame.extend_from_slice(field);
    }
    Some(frame)
}

mod tests {
    use super::*;
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
            assert!(!sign::verify_detached(&signature, &frame, &public), "{name}: changed kind");
            verified_kinds.push(kind);
        }
        verified_kinds.sort_unstable();
        verified_kinds.dedup();
        assert_eq!(verified_kinds, [1, 2, 3, 4, 5, 6]);
        assert_eq!(rejected, 17);
    }
}
