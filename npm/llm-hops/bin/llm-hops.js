#!/usr/bin/env node
'use strict';

// Runs the llm-hops binary for this machine. The binary comes from one of the platform packages
// (llm-hops-darwin-arm64, ...), which npm installs as an optional dependency by `os` and `cpu`.
// Nothing is downloaded here and nothing is written to stdout: `llm-hops mcp` speaks JSON-RPC on
// stdout, so every message from this file goes to stderr.

const { spawn } = require('node:child_process');
const fs = require('node:fs');

const PLATFORMS = {
  'darwin arm64': 'llm-hops-darwin-arm64',
  'darwin x64': 'llm-hops-darwin-x64',
  'linux arm64': 'llm-hops-linux-arm64',
  'linux x64': 'llm-hops-linux-x64',
};

function fail(lines) {
  process.stderr.write('llm-hops: ' + lines.join('\n  ') + '\n');
  process.exit(1);
}

function findBinary() {
  const override = process.env.LLM_HOPS_BINARY;
  if (override) {
    if (!fs.existsSync(override)) fail([`LLM_HOPS_BINARY is set to ${override}, which does not exist.`]);
    return override;
  }
  const key = `${process.platform} ${process.arch}`;
  const pkg = PLATFORMS[key];
  if (!pkg) {
    fail([
      `the npm package has no binary for ${process.platform} ${process.arch}.`,
      'It supports macOS and Linux on arm64 and x64.',
      'Build it from source instead: go install github.com/YauhenBichel/llm-hops/cmd/llm-hops@latest',
      'Or point LLM_HOPS_BINARY at a binary you built.',
    ]);
  }
  try {
    return require.resolve(`${pkg}/bin/llm-hops`);
  } catch (err) {
    fail([
      `the package ${pkg}, which holds the binary for this machine, is not installed.`,
      'npm installs it as an optional dependency. It is skipped by --omit=optional (or --no-optional),',
      'and by a node_modules folder copied from a machine of another kind.',
      'Install again without that flag, or point LLM_HOPS_BINARY at a binary you built.',
    ]);
  }
}

const child = spawn(findBinary(), process.argv.slice(2), { stdio: 'inherit' });

// The child shares this process's stdin, stdout and stderr. Signals sent to this process alone
// (an MCP client stopping its server, `kill`, a service manager) are passed on, so the binary shuts
// down the way it would without the launcher. SIGKILL and SIGSTOP cannot be caught.
const SIGNALS = ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGQUIT', 'SIGUSR1', 'SIGUSR2', 'SIGPIPE'];
const handlers = {};
for (const sig of SIGNALS) {
  handlers[sig] = () => {
    try {
      child.kill(sig);
    } catch (err) {
      // the child has already gone
    }
  };
  process.on(sig, handlers[sig]);
}

child.on('error', (err) => {
  fail([`could not start ${child.spawnfile}: ${err.message}`]);
});

child.on('exit', (code, signal) => {
  if (signal) {
    // End the same way the binary did, so a caller sees the signal and not an exit code.
    for (const sig of SIGNALS) process.removeListener(sig, handlers[sig]);
    process.kill(process.pid, signal);
    // Not reached for a signal that ends the process; the usual shell code for the rest.
    setTimeout(() => process.exit(128 + (require('node:os').constants.signals[signal] || 0)), 100);
    return;
  }
  process.exit(code === null ? 1 : code);
});
