// Package web embeds the platform's browser assets.
package web

import "embed"

//go:embed index.html usage.js snapshot.js snapshot-loader.js snapshot-worker.js app.js platform.js settings.js agent.js style.css
var Assets embed.FS
