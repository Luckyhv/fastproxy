// PM2 config for the Go proxy.
// Unlike the Bun proxy, this is a compiled binary — PM2 runs it directly with no
// interpreter. Go already uses all cores via goroutines, so we run ONE instance
// in fork mode (cluster mode buys nothing here and would just fight over the
// port).
//
// Build first:   go build -o fastproxy .
// Start:         pm2 start ecosystem.config.js
// Persist:       pm2 save && pm2 startup   (run the command it prints)

module.exports = {
  apps: [
    {
      name: "fastproxy",
      script: "./fastproxy", // the compiled binary in this dir
      cwd: __dirname,
      exec_mode: "fork",
      instances: 1,

      // Safety net only. Go's backpressure bounds RAM to ~64 KB per live stream,
      // so the real working set is small (tens of MB even under heavy load) — this
      // ceiling should essentially never fire. If it does, something regressed.
      max_memory_restart: "2G",

      autorestart: true,
      max_restarts: 20,
      min_uptime: "10s",
      restart_delay: 2000,

      // Config lives in ./.env (the binary loads it on startup). Anything set
      // here in `env` overrides the .env file, since real env vars win.
      env: {},
    },
  ],
};
