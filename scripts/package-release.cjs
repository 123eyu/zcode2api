'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const [goos, goarch, version = 'ci'] = process.argv.slice(2);
assert.ok(['windows', 'linux', 'darwin'].includes(goos));
assert.ok(['amd64', 'arm64'].includes(goarch));
assert.match(version, /^[A-Za-z0-9._-]+$/);
const root = path.resolve(__dirname, '..');
const output = path.join(root, 'dist');
const name = `zcode2api_${version}_${goos}_${goarch}`;
const directory = path.join(output, name);
assert.ok(!fs.existsSync(directory), 'Package output already exists');
fs.mkdirSync(directory, { recursive: true });
const binary = goos === 'windows' ? 'zcode2api.exe' : 'zcode2api';
execFileSync('go', ['build', '-trimpath', '-ldflags', `-s -w -X main.version=${version}`, '-o', path.join(directory, binary), '.'], { cwd: root, env: { ...process.env, CGO_ENABLED: '0', GOOS: goos, GOARCH: goarch }, stdio: 'inherit' });
const bytes = fs.readFileSync(path.join(directory, binary));
if (goos === 'windows') {
  assert.equal(bytes.toString('ascii', 0, 2), 'MZ');
  assert.equal(bytes.readUInt16LE(bytes.readUInt32LE(0x3c) + 4), goarch === 'amd64' ? 0x8664 : 0xaa64);
} else if (goos === 'linux') {
  assert.equal(bytes.toString('hex', 0, 4), '7f454c46');
  assert.equal(bytes.readUInt16LE(18), goarch === 'amd64' ? 62 : 183);
} else {
  assert.equal(bytes.readUInt32LE(0), 0xfeedfacf);
  assert.equal(bytes.readUInt32LE(4), goarch === 'amd64' ? 0x01000007 : 0x0100000c);
}
const files = [
  'LICENSE', 'README.md', 'README.EN.md', 'README.JP.md', 'CHANGELOG.md', 'SECURITY.md', 'config.example.json',
  'compose.yaml', 'compose.update.yaml',
  'docs/configuration.md', 'docs/network.md',
  'docs/protocol-compatibility.md', 'grammar_python/requirements.txt',
  'captcha_node/package.json', 'captcha_node/package-lock.json',
  ...fs.readdirSync(path.join(root, 'docs')).filter(file => file.endsWith('.png')).map(file => `docs/${file}`),
];
if (goos === 'windows') files.push('scripts/start.ps1', 'scripts/stop.ps1');
for (const file of files) {
  const destination = path.join(directory, file);
  fs.mkdirSync(path.dirname(destination), { recursive: true });
  fs.copyFileSync(path.join(root, file), destination);
}
const archive = path.join(output, name + (goos === 'windows' ? '.zip' : '.tar.gz'));
if (goos === 'windows' && process.platform === 'win32') {
  execFileSync('powershell.exe', ['-NoProfile', '-Command', '$ErrorActionPreference = "Stop"; Compress-Archive -LiteralPath $env:Z2A_PACKAGE_DIRECTORY -DestinationPath $env:Z2A_PACKAGE_ARCHIVE'], { env: { ...process.env, Z2A_PACKAGE_DIRECTORY: directory, Z2A_PACKAGE_ARCHIVE: archive } });
} else if (goos === 'windows') execFileSync('zip', ['-qr', archive, name], { cwd: output });
else execFileSync('tar', ['-czf', archive, name], { cwd: output });
// Inspect extracted names through Node: native archive listings use the host's
// code page on Windows and corrupt non-ASCII paths when decoded as UTF-8.
const checkDirectory = fs.mkdtempSync(path.join(output, '.verify-'));
assert.equal(path.dirname(checkDirectory), output);
try {
  if (goos === 'windows' && process.platform !== 'win32') execFileSync('unzip', ['-q', archive, '-d', checkDirectory]);
  else execFileSync('tar', ['-xf', archive, '-C', checkDirectory]);
  const entries = fs.readdirSync(checkDirectory, { recursive: true }).map(entry => entry.replaceAll('\\', '/'));
  for (const file of [binary, ...files]) assert.ok(entries.includes(`${name}/${file}`), `Archive missing ${file}`);
  assert.ok(entries.every(entry => (entry === name || entry.startsWith(name + '/')) && !/(^|\/)(data|node_modules|\.git)(\/|$)/.test(entry) && !entry.endsWith('/config.json')), 'Archive contains runtime data or an unexpected path');
} finally {
  fs.rmSync(checkDirectory, { recursive: true, force: true });
}
console.log(`Verified release archive, required files and binary architecture: ${archive}`);
