import { describe, expect, it } from 'vitest';
import { megabytes, readMegabytes } from './format';

describe('megabyte settings inputs', () => {
  it('inherits only when an optional size is blank', () => {
    expect(readMegabytes('', true)).toBeNull();
    expect(readMegabytes('  ', true)).toBeNull();
    expect(readMegabytes('1.25', true)).toBe(1_250_000);
    expect(() => readMegabytes('0', true)).toThrow();
    expect(() => readMegabytes('invalid', true)).toThrow();
  });
  it.each([
    ['100', 100_000_000],
    ['2000', 2_000_000_000],
    ['1.5', 1_500_000],
    ['.25', 250_000],
    ['0.000001', 1],
    ['1.000001', 1_000_001],
    ['1.000003', 1_000_003],
    [' 001.25 ', 1_250_000],
    ['9007199254.740991', Number.MAX_SAFE_INTEGER],
  ])('converts %s MB to exactly %i bytes', (input, expected) => {
    expect(readMegabytes(input)).toBe(expected);
  });

  it.each([1, 1024, 1_000_000, 1_073_741_824, 10_737_418_240, Number.MAX_SAFE_INTEGER])(
    'preserves an existing %i-byte setting without rounding',
    value => expect(readMegabytes(megabytes(value))).toBe(value),
  );

  it('formats decimal values without unnecessary trailing zeros', () => {
    expect(megabytes(1_073_741_824)).toBe('1073.741824');
    expect(megabytes(10_737_418_240)).toBe('10737.41824');
    expect(megabytes(100_000_000)).toBe('100');
  });

  it.each(['', ' ', '0', '-1', 'NaN', 'Infinity', '1e3', '1,000', '0.0000001', '1.1234567', '9007199254.740992'])(
    'rejects invalid or unrepresentable input %j',
    input => expect(() => readMegabytes(input)).toThrow(),
  );
});
