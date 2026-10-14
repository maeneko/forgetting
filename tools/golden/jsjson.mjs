// Эталон для internal/jsjson: вывод JSON.stringify самого Node на каверзных значениях.
// Запуск: node tools/golden/jsjson.mjs > testdata/golden/jsjson.json
const strings = [
  "", "plain", "кириллица", "emoji 😀", "<r 2><b 0x85>", "a&b<c>d", "slash/и\\обратный",
  "quote\"q", "line\nbreak", "tab\tcr\r", "\b\f", "\u0000\u0001\u001f\u007f", "  ",
  "I2 = ", "  spaces  ", "ü", "日本語",
];
const numbers = [0, -0, 1, -1, 1376, 47619, 0.1, 0.5, 1.5, 2.5, 100.25, 1e21, 1e20, 123456789012345680000,
  1e-7, 1e-6, 0.000001234, 1.7976931348623157e308, 5e-324, 3.14159, -273.15, 2 ** 53, 2 ** 53 + 2, 0.1 + 0.2, 250.5,
  NaN, Infinity, -Infinity];
const objects = [
  { a: 1, b: "x", c: [1, 2, { d: null }], e: true, f: false },
  { rekey: false, endpoints: ["1.2.3.4:443"], servers: [{ id: 0, name: "VPN & <co> ", awg: { Jc: "6", I1: "<r 2>" } }] },
  { empty: {}, arr: [], nested: { x: [] } },
  [],
  {},
];
const out = {
  strings: strings.map(s => ({ in: s, out: JSON.stringify(s) })),
  numbers: numbers.map(n => ({ in: Number.isFinite(n) ? n : String(n), out: JSON.stringify(n), str: String(n) })),
  objects: objects.map(o => ({ in: o, compact: JSON.stringify(o), indent2: JSON.stringify(o, null, 2) })),
};
process.stdout.write(JSON.stringify(out, null, 1));
