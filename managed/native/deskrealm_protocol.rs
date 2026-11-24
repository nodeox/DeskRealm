//! DeskRealm signing frames and the transport binding used by native managed peers.

use sodiumoxide::crypto::{auth::hmacsha256, hash::sha256, sign};
use std::convert::TryInto;

pub const DEVICE_CHALLENGE: u8 = 1;
pub const USER_BINDING: u8 = 2;
pub const SESSION_GRANT: u8 = 3;
pub const CONNECTION_PROOF: u8 = 4;
pub const POLICY_RECEIPT: u8 = 5;
pub const SESSION_CONSENT: u8 = 6;
pub const RELAY_ROUND_JOIN: u8 = 7;
pub const RELAY_PROBE: u8 = 8;
pub const RELAY_PROBE_PERMIT: u8 = 9;
pub const RELAY_PROBE_HOLDER: u8 = 10;

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

fn valid_values(kind: u8, f: &[&[u8]]) -> bool {
    match kind {
        DEVICE_CHALLENGE => positive(&f[4]) && positive(&f[5]) && f[6] != [0, 0],
        USER_BINDING => positive(&f[6]) && positive(&f[7]),
        SESSION_GRANT => {
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
        CONNECTION_PROOF => {
            (1..=2).contains(&f[0][0]) && (1..=2).contains(&f[7][0]) && f[5] != f[6]
        }
        POLICY_RECEIPT => {
            positive(&f[3])
                && positive(&f[5])
                && ((f[6][0] == 1 && f[7][0] == 0) || (f[6][0] == 2 && (1..=7).contains(&f[7][0])))
        }
        SESSION_CONSENT => (1..=0x1f).contains(&f[4][0]),
        RELAY_ROUND_JOIN => (1..=2).contains(&f[0][0]),
        RELAY_PROBE => {
            (1..=2).contains(&f[0][0])
                && positive(f[6])
                && f[8].chunks_exact(5).all(|sample| {
                    let rtt = u32::from_be_bytes(sample[1..].try_into().expect("validated RTT width"));
                    match sample[0] {
                        1 => rtt <= 2_000_000,
                        2..=5 => rtt == 0,
                        _ => false,
                    }
                })
        }
        RELAY_PROBE_PERMIT => {
            let (start, expires) = (number(f[9]), number(f[10]));
            (1..=2).contains(&f[0][0])
                && positive(f[6])
                && positive(f[9])
                && positive(f[10])
                && expires > start
                && expires - start <= 2000
        }
        RELAY_PROBE_HOLDER => f[1][0] < 3,
        _ => false,
    }
}

fn widths(kind: u8) -> Option<&'static [usize]> {
    Some(match kind {
        DEVICE_CHALLENGE => &[16, 16, 16, 32, 8, 8, 2],
        USER_BINDING => &[16, 16, 16, 16, 32, 32, 8, 8],
        SESSION_GRANT => &[0, 16, 16, 16, 16, 16, 16, 32, 32, 8, 1, 1, 0, 8, 8],
        CONNECTION_PROOF => &[1, 32, 16, 16, 32, 32, 32, 1],
        POLICY_RECEIPT => &[16, 16, 16, 8, 32, 8, 1, 1],
        SESSION_CONSENT => &[32, 16, 16, 32, 1],
        RELAY_ROUND_JOIN => &[1, 16, 16, 16],
        RELAY_PROBE => &[1, 16, 16, 16, 16, 16, 8, 32, 15],
        RELAY_PROBE_PERMIT => &[1, 16, 16, 16, 16, 16, 8, 32, 32, 8, 8],
        RELAY_PROBE_HOLDER => &[32, 1, 32],
        _ => return None,
    })
}

pub const SESSION_PROBE_FRAME_SIZE: usize = 199;
pub const SESSION_PROBE_PREFIX: &[u8] = b"DeskRealm/relay/v1/rtt-1\0";

pub struct RelayProbePermit {
    pub node: [u8; 16],
    pub version: u64,
    pub endpoint: sha256::Digest,
    pub holder: sign::PublicKey,
    pub opens: u64,
    pub expires: u64,
}

impl RelayProbePermit {
    pub fn verify(frame: &[u8], signature: &[u8], key: &sign::PublicKey) -> Option<Self> {
        let fields = decode(RELAY_PROBE_PERMIT, frame)?;
        let signature = sign::Signature::from_bytes(signature).ok()?;
        if !sign::verify_detached(&signature, frame, key) {
            return None;
        }
        Some(Self {
            node: fields[5].try_into().ok()?,
            version: number(fields[6]),
            endpoint: sha256::Digest::from_slice(fields[7])?,
            holder: sign::PublicKey::from_slice(fields[8])?,
            opens: number(fields[9]),
            expires: number(fields[10]),
        })
    }

    pub fn active(&self, now_ms: u64) -> bool {
        self.opens <= now_ms && now_ms < self.expires
    }
}

fn valid_fields(kind: u8, fields: &[&[u8]]) -> bool {
    let Some(widths) = widths(kind) else {
        return false;
    };
    fields.len() == widths.len()
        && fields
            .iter()
            .zip(widths)
            .enumerate()
            .all(|(index, (field, width))| {
                if *width == 0 {
                    identifier(field, if index == 12 { 64 } else { 32 })
                } else {
                    field.len() == *width
                }
            })
        && valid_values(kind, fields)
}

pub fn encode(kind: u8, fields: &[&[u8]]) -> Option<Vec<u8>> {
    if !valid_fields(kind, fields) {
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

fn decode(kind: u8, frame: &[u8]) -> Option<Vec<&[u8]>> {
    let mut remaining = frame.strip_prefix(PREFIX)?.strip_prefix(&[kind])?;
    let expected = widths(kind)?.len();
    let mut fields = Vec::with_capacity(expected);
    for _ in 0..expected {
        let width = u16::from_be_bytes(remaining.get(..2)?.try_into().ok()?) as usize;
        fields.push(remaining.get(2..2 + width)?);
        remaining = &remaining[2 + width..];
    }
    (remaining.is_empty() && valid_fields(kind, &fields)).then_some(fields)
}

#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum PeerRole {
    Controller = 1,
    Target = 2,
}

#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum Path {
    Direct = 1,
    Relay = 2,
}

/// Kept opaque so an application uses a digest from the local encrypted stream.
#[derive(Clone, Copy, PartialEq, Eq)]
pub struct ChannelBinding([u8; 32]);

impl ChannelBinding {
    pub fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }
}

/// Export a context-bound value from a negotiated directional RustDesk stream.
/// The unexported master secret is the HMAC key, never included in the wire.
pub fn channel_binding(
    secret: &[u8; 32],
    initiator_ephemeral: &[u8],
    responder_ephemeral: &[u8],
    offered: u32,
    selected: u32,
) -> Option<ChannelBinding> {
    if selected != 1
        || offered < selected
        || initiator_ephemeral.len() != 32
        || responder_ephemeral.len() != 32
        || initiator_ephemeral == responder_ephemeral
    {
        return None;
    }
    let mut mac = hmacsha256::State::init(secret);
    mac.update(b"DeskRealm/M2/RustDesk-channel/v1\0");
    mac.update(&offered.to_be_bytes());
    mac.update(&selected.to_be_bytes());
    mac.update(initiator_ephemeral);
    mac.update(responder_ephemeral);
    Some(ChannelBinding(mac.finalize().0))
}

pub struct VerifiedGrant<'a> {
    fields: Vec<&'a [u8]>,
    digest: [u8; 32],
}

impl<'a> VerifiedGrant<'a> {
    /// The configured center key and key ID are trust inputs from the caller.
    pub fn verify(
        frame: &'a [u8],
        signature: &[u8],
        pinned_kid: &str,
        center_key: &sign::PublicKey,
        now_ms: u64,
    ) -> Option<Self> {
        let fields = decode(SESSION_GRANT, frame)?;
        if fields[0] != pinned_kid.as_bytes()
            || now_ms < number(fields[13])
            || now_ms >= number(fields[14])
            || !verify_signature(frame, signature, center_key)
        {
            return None;
        }
        let mut state = sha256::State::new();
        state.update(frame);
        state.update(signature);
        Some(Self {
            fields,
            digest: state.finalize().0,
        })
    }

    pub fn organization_id(&self) -> &[u8] {
        self.fields[1]
    }

    pub fn session_id(&self) -> &[u8] {
        self.fields[2]
    }

    pub fn grant_id(&self) -> &[u8] {
        self.fields[3]
    }

    pub fn user_id(&self) -> &[u8] {
        self.fields[4]
    }

    pub fn device_id(&self, role: PeerRole) -> &[u8] {
        self.fields[if role == PeerRole::Controller { 5 } else { 6 }]
    }

    pub fn matches_device_key(&self, role: PeerRole, key: &sign::PublicKey) -> bool {
        self.fields[if role == PeerRole::Controller { 7 } else { 8 }] == sha256::hash(&key.0).0
    }

    pub fn policy_version(&self) -> u64 {
        number(self.fields[9])
    }

    pub fn allowed_mask(&self) -> u8 {
        self.fields[10][0]
    }

    pub fn attended(&self) -> bool {
        self.fields[11][0] == 1
    }

    pub fn purpose(&self) -> &str {
        std::str::from_utf8(self.fields[12]).expect("validated ASCII purpose")
    }

    pub fn expires_ms(&self) -> u64 {
        number(self.fields[14])
    }

    pub fn bind<'g>(
        &'g self,
        connection_id: [u8; 16],
        channel: ChannelBinding,
        controller_nonce: [u8; 32],
        target_nonce: [u8; 32],
        path: Path,
    ) -> Option<BoundConnection<'g, 'a>> {
        if controller_nonce == target_nonce {
            return None;
        }
        Some(BoundConnection {
            grant: self,
            connection_id,
            channel,
            controller_nonce,
            target_nonce,
            path,
        })
    }
}

pub struct BoundConnection<'g, 'wire> {
    grant: &'g VerifiedGrant<'wire>,
    connection_id: [u8; 16],
    channel: ChannelBinding,
    controller_nonce: [u8; 32],
    target_nonce: [u8; 32],
    path: Path,
}

impl BoundConnection<'_, '_> {
    pub fn proof_frame(&self, role: PeerRole) -> Vec<u8> {
        encode(
            CONNECTION_PROOF,
            &[
                &[role as u8],
                &self.grant.digest,
                self.grant.session_id(),
                &self.connection_id,
                self.channel.as_bytes(),
                &self.controller_nonce,
                &self.target_nonce,
                &[self.path as u8],
            ],
        )
        .expect("validated connection fields")
    }

    pub fn verify_peer(&self, role: PeerRole, key: &sign::PublicKey, signature: &[u8]) -> bool {
        self.grant.matches_device_key(role, key)
            && verify_signature(&self.proof_frame(role), signature, key)
    }

    pub fn consent_frame(&self, confirmed_mask: u8) -> Option<Vec<u8>> {
        if !self.grant.attended()
            || confirmed_mask == 0
            || confirmed_mask & !self.grant.allowed_mask() != 0
        {
            return None;
        }
        encode(
            SESSION_CONSENT,
            &[
                &self.grant.digest,
                self.grant.session_id(),
                &self.connection_id,
                self.channel.as_bytes(),
                &[confirmed_mask],
            ],
        )
    }

    pub fn verifies_consent(
        &self,
        key: &sign::PublicKey,
        confirmed_mask: u8,
        signature: &[u8],
    ) -> bool {
        self.grant.matches_device_key(PeerRole::Target, key)
            && self
                .consent_frame(confirmed_mask)
                .map_or(false, |frame| verify_signature(&frame, signature, key))
    }
}

fn verify_signature(message: &[u8], signature: &[u8], public_key: &sign::PublicKey) -> bool {
    sign::Signature::from_bytes(signature)
        .map(|sig| sign::verify_detached(&sig, message, public_key))
        .unwrap_or(false)
}

/// These purposes separate rendezvous initiation, target replies and relay joins.
#[derive(Clone, Copy)]
#[repr(u8)]
pub enum TransportPurpose {
    DirectRequest = 1,
    RelayRequest = 2,
    DirectReply = 3,
    RelayReply = 4,
    RelayJoin = 5,
}

impl VerifiedGrant<'_> {
    pub fn transport_proof_frame(
        &self,
        purpose: TransportPurpose,
        role: PeerRole,
        connection: &[u8; 16],
        target_rustdesk_id: &str,
        challenge: &[u8; 32],
        offer: &[u8; 32],
    ) -> Vec<u8> {
        let mut frame = b"DeskRealm/M2/transport-proof/v1\0".to_vec();
        frame.extend_from_slice(&[purpose as u8, role as u8]);
        frame.extend_from_slice(&self.digest);
        frame.extend_from_slice(connection);
        frame.extend_from_slice(challenge);
        frame.extend_from_slice(offer);
        // The sole variable-length field is last. Its ASCII form is checked
        // by the transport ingress before a route can reach the center.
        frame.extend_from_slice(target_rustdesk_id.as_bytes());
        frame
    }
}

/// Check the RustDesk routing address separately from the stable signed UUID.
pub fn valid_rustdesk_id(value: &str) -> bool {
    let bytes = value.as_bytes();
    (3..=32).contains(&bytes.len())
        && bytes
            .iter()
            .enumerate()
            .all(|(i, b)| b.is_ascii_alphanumeric() || (i > 0 && matches!(b, b'-' | b'_')))
}
