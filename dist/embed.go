// Package web embeds the platform's browser assets.
package web

import "embed"

//go:embed gpu.js gpu.css clipboard.js status.html status.js status.css index.html usage.js snapshot.js snapshot-loader.js app.js platform.js settings.js updates.js agent.js cleanup.js dashboard.js process.js containers.js members.js bastion.js cluster.js scan-schedule.js cluster.css style.css workspace.css auth.css auth.js
var Assets embed.FS
