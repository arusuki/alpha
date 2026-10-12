// Package web embeds the platform's browser assets.
package web

import "embed"

//go:embed disk-capacity.js disk-capacity.css gpu.js gpu.css member-disk.js clipboard.js status.html status.js status.css index.html usage.js snapshot.js snapshot-loader.js app.js platform.js settings.js updates.js mihomo.js agent.js assistant.js assistant.css cleanup.js dashboard.js process.js containers.js members.js bastion.js cluster.js services.js scan-schedule.js cluster.css style.css workspace.css auth.css auth.js
var Assets embed.FS
