// Package web embeds the platform's browser assets.
package web

import "embed"

//go:embed index.html usage.js snapshot.js snapshot-cache.js snapshot-loader.js snapshot-worker.js app.js platform.js settings.js agent.js cleanup.js dashboard.js process.js containers.js members.js cluster.js cluster.css style.css workspace.css auth.css auth.js
var Assets embed.FS
