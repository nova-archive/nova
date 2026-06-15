// "learn-this" jargon glossary for the Settings screen (ported from web/setup).
// Each entry feeds an <InfoTerm> ⓘ disclosure.
export interface GlossaryEntry {
  label: string // accessible name for the <summary>
  body: string // 1–2 sentence plain-English explanation
}

export const GLOSSARY: Record<string, GlossaryEntry> = {
  paranoid: {
    label: 'What does hardening (paranoid) do?',
    body: 'A preset that turns on every privacy protection at once. You can relax any single protection below; the rest stay on.',
  },
  'source-ip': {
    label: 'What is source-IP recording?',
    body: 'Whether each stored blob keeps the uploader’s IP address. Useful for abuse tracing; a privacy cost to your visitors.',
  },
  'ipfs-dht': {
    label: 'What is the public IPFS DHT?',
    body: 'A global directory mapping content IDs to the nodes that hold them. Joining it publicly advertises which CIDs this node stores.',
  },
  cors: {
    label: 'What does CORS control?',
    body: 'Which web origins a browser will let script this node’s upload endpoint. It is not an access-control boundary — it constrains browsers, not curl.',
  },
  'public-uploads': {
    label: 'What are public uploads?',
    body: 'Letting unauthenticated visitors upload through this node’s widget. Off by default; turning it on widens your abuse surface and requires a Terms-of-Service URL.',
  },
  tos: {
    label: 'Why is a Terms-of-Service URL required?',
    body: 'When anyone can upload, Nova requires a published ToS so visitors see the rules. This is a hard requirement (T1.20) — the node refuses to start without it.',
  },
}
