#!/usr/bin/env node
'use strict';

// Builds the npm packages from what GoReleaser left in dist/: the launcher package `llm-hops` and one
// package per platform holding that platform's binary. Nothing is published from here.
//
//   node npm/build.js [--dist dist] [--out dist/npm] [--version 0.3.0] [--pack]
//
// The version is the one GoReleaser built (dist/metadata.json). With --version, the two must agree, so
// a tag can never publish binaries of another version. Each binary is checked against checksums.txt
// before it is copied. --pack also runs `npm pack` on every package, into the --out folder.
// Standard library only, Node 18 or later.

const { execFileSync } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const ROOT = path.resolve(__dirname, '..');
const MAIN = 'llm-hops';
// GoReleaser's names to npm's: `os` and `cpu` in package.json use Node's process.platform and process.arch.
const TARGETS = [
  { goos: 'darwin', goarch: 'arm64', os: 'darwin', cpu: 'arm64', label: 'macOS on Apple silicon' },
  { goos: 'darwin', goarch: 'amd64', os: 'darwin', cpu: 'x64', label: 'macOS on Intel' },
  { goos: 'linux', goarch: 'arm64', os: 'linux', cpu: 'arm64', label: 'Linux on arm64' },
  { goos: 'linux', goarch: 'amd64', os: 'linux', cpu: 'x64', label: 'Linux on x64' },
];

function die(message) {
  process.stderr.write(`npm/build.js: ${message}\n`);
  process.exit(1);
}

function options(argv) {
  const opts = { dist: path.join(ROOT, 'dist'), out: null, version: null, pack: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === '--pack') opts.pack = true;
    else if (arg === '--dist' || arg === '--out' || arg === '--version') {
      if (argv[i + 1] === undefined) die(`${arg} needs a value`);
      opts[arg.slice(2)] = argv[++i];
    } else die(`unknown argument ${arg}`);
  }
  opts.dist = path.resolve(opts.dist);
  opts.out = path.resolve(opts.out || path.join(opts.dist, 'npm'));
  return opts;
}

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (err) {
    die(`cannot read ${file}: ${err.message}. Run \`make snapshot\` (or GoReleaser) first.`);
  }
}

function sha256(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
}

function writeJSON(file, value) {
  fs.writeFileSync(file, JSON.stringify(value, null, 2) + '\n');
}

const opts = options(process.argv.slice(2));
const metadata = readJSON(path.join(opts.dist, 'metadata.json'));
const artifacts = readJSON(path.join(opts.dist, 'artifacts.json'));

const built = String(metadata.version || '');
const wanted = (opts.version || built).replace(/^v/, '');
if (wanted !== built) {
  die(`the version asked for is ${wanted}, but dist/ holds the binaries of ${built}. Nothing was built.`);
}
// npm ignores build metadata (0.3.1-snapshot+abc1234 is published as 0.3.1-snapshot), so drop it.
const version = built.replace(/\+.*$/, '');
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) die(`"${built}" is not a version npm accepts`);

const checksumsFile = path.join(opts.dist, 'checksums.txt');
const checksums = new Map();
try {
  for (const line of fs.readFileSync(checksumsFile, 'utf8').split('\n')) {
    const m = line.match(/^([0-9a-f]{64})\s+\*?(\S+)$/);
    if (m) checksums.set(m[2], m[1]);
  }
} catch (err) {
  die(`cannot read ${checksumsFile}: ${err.message}`);
}

const template = readJSON(path.join(__dirname, MAIN, 'package.json'));
const licence = path.join(ROOT, 'LICENSE');

fs.rmSync(opts.out, { recursive: true, force: true });
fs.mkdirSync(opts.out, { recursive: true });

const dirs = [];
const optionalDependencies = {};

for (const t of TARGETS) {
  const name = `${MAIN}-${t.os}-${t.cpu}`;
  // The bare binary as the release publishes it (llm-hops-linux-amd64), which is the name in checksums.txt.
  const released = `${MAIN}-${t.goos}-${t.goarch}`;
  const artifact = artifacts.find((a) => a.goos === t.goos && a.goarch === t.goarch && a.name === released);
  if (!artifact) die(`dist/artifacts.json has no ${released}`);
  const source = path.resolve(opts.dist, '..', artifact.path);
  const want = checksums.get(released);
  if (!want) die(`checksums.txt has no line for ${released}`);
  const got = sha256(source);
  if (got !== want) die(`${source} has SHA-256 ${got}, but checksums.txt says ${want}`);

  const dir = path.join(opts.out, name);
  fs.mkdirSync(path.join(dir, 'bin'), { recursive: true });
  fs.copyFileSync(source, path.join(dir, 'bin', MAIN));
  fs.chmodSync(path.join(dir, 'bin', MAIN), 0o755);
  fs.copyFileSync(licence, path.join(dir, 'LICENSE'));
  fs.writeFileSync(
    path.join(dir, 'README.md'),
    `# ${name}\n\nThe llm-hops ${version} binary for ${t.label}` +
      ` (SHA-256 \`${got}\`, the same file as \`${released}\` on the\n` +
      `[GitHub release](https://github.com/YauhenBichel/llm-hops/releases)).\n\n` +
      `Do not install this package directly. Install [\`${MAIN}\`](https://www.npmjs.com/package/${MAIN}),` +
      ` which picks the package for your machine:\n\n` +
      '```bash\nnpm install -g llm-hops\n```\n'
  );
  writeJSON(path.join(dir, 'package.json'), {
    name,
    version,
    description: `The llm-hops binary for ${t.label}. Install llm-hops instead.`,
    homepage: template.homepage,
    bugs: template.bugs,
    repository: template.repository,
    license: template.license,
    os: [t.os],
    cpu: [t.cpu],
    files: ['bin/llm-hops'],
    // No "bin": the launcher in `llm-hops` owns the command name and finds this file by path.
  });
  optionalDependencies[name] = version;
  dirs.push(dir);
}

const declared = Object.keys(template.optionalDependencies || {}).sort().join(' ');
if (declared !== Object.keys(optionalDependencies).sort().join(' ')) {
  die(`npm/${MAIN}/package.json lists other platform packages (${declared}) than this script builds`);
}

const mainDir = path.join(opts.out, MAIN);
fs.mkdirSync(path.join(mainDir, 'bin'), { recursive: true });
fs.copyFileSync(path.join(__dirname, MAIN, 'bin', 'llm-hops.js'), path.join(mainDir, 'bin', 'llm-hops.js'));
fs.chmodSync(path.join(mainDir, 'bin', 'llm-hops.js'), 0o755);
fs.copyFileSync(path.join(__dirname, MAIN, 'README.md'), path.join(mainDir, 'README.md'));
fs.copyFileSync(licence, path.join(mainDir, 'LICENSE'));
writeJSON(path.join(mainDir, 'package.json'), { ...template, version, optionalDependencies });
dirs.push(mainDir);

for (const dir of dirs) {
  if (opts.pack) {
    execFileSync('npm', ['pack', '--silent', '--pack-destination', opts.out], { cwd: dir, stdio: ['ignore', 'ignore', 'inherit'] });
  }
  process.stderr.write(`${path.basename(dir)} ${version}\n`);
}
process.stderr.write(`${opts.pack ? 'packages and tarballs' : 'packages'} in ${opts.out}\n`);
