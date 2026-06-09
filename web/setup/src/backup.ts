// buildBackupContent returns the exact text of the master-key backup file the
// wizard offers for download on the master-key step. It contains the key + its
// fingerprint and NOTHING ELSE sensitive — no answers, no admin password. Pure
// (no DOM/Blob) so it is trivially unit-testable; the wizard wraps it in a Blob
// + temporary <a download> click.
export function buildBackupContent(masterKeyHex: string, fingerprint: string): string {
  return `Nova master key — store this offline; it cannot be recovered.\nmaster_key: ${masterKeyHex}\nfingerprint: ${fingerprint}\n`
}
