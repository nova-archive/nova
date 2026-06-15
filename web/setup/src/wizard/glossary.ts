// The "learn-this" jargon glossary: terms an operator must understand to run the
// node safely. Each entry feeds an <InfoTerm> ⓘ disclosure. Abstract-away terms
// (swarm/signing keys, sealing, replication) intentionally have NO entry — they
// render as muted plain copy with no info button.
export interface GlossaryEntry {
  label: string // accessible name for the <summary>, e.g. "What is a master key?"
  body: string // 1–2 sentence plain-English explanation
}

export const GLOSSARY: Record<string, GlossaryEntry> = {
  'master-key': {
    label: 'What is the master key?',
    body: 'The single secret that encrypts every other secret this node holds. Nova cannot recover it — lose your backup and the sealed keys are gone for good.',
  },
  fingerprint: {
    label: 'What is a key fingerprint?',
    body: 'A short hash of the master key. Typing it back proves you captured the key before setup seals it away.',
  },
  'bootstrap-token': {
    label: 'What is the bootstrap token?',
    body: 'A one-time token printed to the coordinator log. It authorizes this wizard and is required on every setup request until you go live.',
  },
  'tls-mode': {
    label: 'What does TLS mode control?',
    body: 'How this node obtains its HTTPS certificate. Public ACME (http-01) publishes your hostname to Certificate Transparency logs; self-signed and onion modes do not.',
  },
  'public-uploads': {
    label: 'What are public uploads?',
    body: 'Letting unauthenticated visitors upload through this node’s widget. Off by default; turning it on widens your abuse surface and requires a Terms-of-Service URL.',
  },
  tos: {
    label: 'Why is a Terms-of-Service URL required?',
    body: 'When anyone can upload, Nova requires a published ToS so visitors see the rules. This is a hard requirement (T1.20) — setup refuses to finish without it.',
  },
  'source-ip': {
    label: 'What is source-IP recording?',
    body: 'Whether each stored blob keeps the uploader’s IP address. Useful for abuse tracing; a privacy cost to your visitors.',
  },
  'ipfs-dht': {
    label: 'What is the public IPFS DHT?',
    body: 'A global directory mapping content IDs to the nodes that hold them. Joining it publicly advertises which CIDs this node stores.',
  },
  paranoid: {
    label: 'What does hardening (paranoid) do?',
    body: 'A preset that turns on every privacy protection at once. You can relax any single protection below; the rest stay on.',
  },
}
