// Package web embeds the platform's browser assets.
package web

import "embed"

//go:embed index.html usage.js snapshot.js snapshot-cache.js snapshot-loader.js snapshot-worker.js app.js platform.js settings.js agent.js cleanup.js dashboard.js process.js containers.js members.js style.css workspace.css auth.css auth.js workspace-art.png
var Assets embed.FS
