export const CONTRACT_ID_RE = /^C[A-Z2-7]{55}$/;

const BASE32_ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
const CONTRACT_VERSION_BYTE = 0x10;

// Stellar strkey payload layout for a contract id: 1 version byte + 32
// payload bytes + 2 checksum bytes (little-endian) = 35 bytes, base32-encoded
// to 56 chars starting with 'C'.
const STRKEY_CONTRACT_LENGTH = 35;
const STRKEY_PAYLOAD_END = STRKEY_CONTRACT_LENGTH - 2; // 33

function crc16XModem(bytes: Uint8Array): number {
  let crc = 0;
  for (let i = 0; i < bytes.length; i++) {
    crc ^= bytes[i] << 8;
    for (let j = 0; j < 8; j++) {
      crc = crc & 0x8000 ? ((crc << 1) ^ 0x1021) & 0xffff : (crc << 1) & 0xffff;
    }
  }
  return crc;
}

function decodeBase32(value: string): Uint8Array | null {
  let bits = 0;
  let nBits = 0;
  const out: number[] = [];
  for (const ch of value) {
    const v = BASE32_ALPHABET.indexOf(ch);
    if (v === -1) return null;
    bits = (bits << 5) | v;
    nBits += 5;
    if (nBits >= 8) {
      nBits -= 8;
      out.push((bits >> nBits) & 0xff);
    }
  }
  return new Uint8Array(out);
}

/**
 * Full Stellar strkey validation for Soroban contract addresses.
 *
 * This is stricter than the Trident API's format check: it verifies the
 * base32 charset, the contract version byte, and the CRC16 checksum, so a
 * visitor who pastes a typo'd or truncated address gets an honest "that isn't
 * a real contract id" state right away instead of a silent empty result.
 */
export function isValidContractId(value: string): boolean {
  if (!CONTRACT_ID_RE.test(value)) return false;
  const decoded = decodeBase32(value);
  if (!decoded || decoded.length !== STRKEY_CONTRACT_LENGTH) return false;
  if (decoded[0] !== CONTRACT_VERSION_BYTE) return false;

  const expected = crc16XModem(decoded.subarray(0, STRKEY_PAYLOAD_END));
  const low = decoded[STRKEY_PAYLOAD_END];
  const high = decoded[STRKEY_PAYLOAD_END + 1];
  return low === (expected & 0xff) && high === ((expected >> 8) & 0xff);
}