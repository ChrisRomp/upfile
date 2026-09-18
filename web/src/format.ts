export const BRAND = 'upfile';
export const utf8Length = (value: string) => new TextEncoder().encode(value).length;
export function bytes(value: number): string {
  if (!Number.isFinite(value)) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let index = 0;
  let size = value;
  while (size >= 1024 && index < units.length - 1) { size /= 1024; index++; }
  return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: index ? 2 : 0 }).format(size)} ${units[index]}`;
}
export const date = (seconds: number) => new Date(seconds * 1000).toLocaleString(undefined, {
  dateStyle: 'medium', timeStyle: 'short',
});
export function localDateTime(seconds: number): string {
  const value = new Date(seconds * 1000);
  if (!Number.isFinite(value.getTime())) return '';
  return new Date(value.getTime() - value.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}
const BYTES_PER_MEGABYTE = 1_000_000n;

export function megabytes(value: number): string {
  const amount = BigInt(value);
  const fraction = String(amount % BYTES_PER_MEGABYTE).padStart(6, '0').replace(/0+$/, '');
  return `${amount / BYTES_PER_MEGABYTE}${fraction ? `.${fraction}` : ''}`;
}

export function readMegabytes(value: string): number;
export function readMegabytes(value: string, optional: true): number | null;
export function readMegabytes(value: string, optional = false): number | null {
  const input = value.trim();
  if (optional && !input) return null;
  if (!/^(?:\d+(?:\.\d{0,6})?|\.\d{1,6})$/.test(input)) {
    throw new Error('Enter a positive size in MB, with at most six decimal places.');
  }
  const [whole, fraction = ''] = input.split('.');
  // Decimal arithmetic preserves existing limits down to the byte on a round trip.
  const amount = BigInt(whole || '0') * BYTES_PER_MEGABYTE + BigInt(fraction.padEnd(6, '0'));
  if (amount <= 0n || amount > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`Enter a size between 0.000001 and ${megabytes(Number.MAX_SAFE_INTEGER)} MB.`);
  }
  return Number(amount);
}
