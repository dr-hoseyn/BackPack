# Native xGRE compatibility profile

Select **Setup Iran → Standard Kernel L3 / L2 → xgre**, then apply the setup
link on Kharej. This is an independent implementation of the native,
unencrypted xGRE data plane observed on BackPack Premium 0.1.0-dev. It uses
Backpack's existing Linux kernel tunnel lifecycle; it does not include or
redistribute the proprietary BPP binary or its source.

## Observed profile and implementation

- IPv4 underlay, Linux `gre` interface, IP protocol 47.
- RFC 2890 key present in both directions, using all 32 bits; no GRE checksum
  or sequence extension by default.
- TTL 255, inherited outer TOS, PMTU discovery disabled.
- Default inner MTU 1472, appropriate for a 1500-byte IPv4 underlay with a
  keyed GRE header. Set a smaller MTU when the actual underlay requires it.
- `ignore-df` enabled on Iran and disabled on Kharej by default. The optional
  local `gre_ignore_df` boolean overrides this. A generated peer configuration
  recomputes its own role default rather than copying the local override.
- Loose reverse-path filtering (`rp_filter=2`) and forwarding enabled only on
  the newly created interface. These settings disappear when that interface
  is removed. Global forwarding, NAT, firewall rules and default routes are
  left to the operator, as with other kernel tunnels.

This profile does not claim compatibility with BPP's optional encrypted modes,
`xgreip`, its control channel, setup links or secret-to-key derivation.
For an existing peer, read its actual key with `ip -d link show dev INTERFACE`
and enter that value as `id`. Do not assume two applications derive the same
key from a shared secret. The GRE key is a traffic selector, not authentication
or encryption. Keep authenticated encryption in the application carried over
the interface, such as `tcpfwd-xmpp`.

## Standalone underlay example

Iran:

```toml
[kernel]
method = "xgre"
role = "iran"
local_addr = "192.0.2.10"
remote_addr = "198.51.100.20"
local_ip = "10.77.80.1/30"
peer_ip = "10.77.80.2"
interface = "bpkxgre1"
token = "REPLACE_WITH_A_RANDOM_SHARED_SECRET_OF_48_CHARACTERS"
id = 4045620583
mtu = 1472
gre_ignore_df = true
ports = []
```

Kharej swaps the underlay and inner addresses, uses `role = "kharej"`, the
same `id` and MTU, and `gre_ignore_df = false`. A Backpack setup link performs
that pairing. `id = 0` derives a Backpack-specific key from the token; use an
explicit key to interoperate with an existing native GRE endpoint.

An existing BPP-owned interface is not adopted or replaced. Use a distinct
interface, subnet and key when testing alongside another tunnel. Creation
failures unwind only resources created by the new Backpack instance.

## Carry an existing XMPP tunnel

Leave the underlay's `ports` empty. Keep the XMPP server listening on its
existing control port and change the XMPP client's `remote_addr` to the Iran
endpoint's private tunnel address, for example `10.77.80.1:2121`. Keep its
`transport = "tcpfwd-xmpp"`, shared token and user-facing port mappings.

For managed services, order the XMPP unit after the underlay unit with a
systemd drop-in using `Wants=` and `After=`. An optional public-address fallback
can retain connectivity if the private route fails. Measure verified traffic
on the deployed path; native GRE availability and performance vary by network.

The normal kernel reverse option remains a Backpack-specific authenticated TCP
carrier around this profile. Only native mode matches the observed BPP data
plane described above.
