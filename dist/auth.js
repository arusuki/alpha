'use strict';

// The alpha is one continuous nib stroke. Its outline grows with the pen;
// revealing a finished image through a mask would expose the crossing too early.
(() => {
  const panel = document.getElementById('authPanel');
  const card = document.getElementById('authCard');
  const guide = document.getElementById('authAlphaGuide');
  const ink = document.getElementById('authAlphaInk');
  const tip = document.getElementById('authAlphaTip');
  const skip = document.getElementById('authSkip');
  const replay = document.getElementById('authReplay');
  const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
  const duration = { lead: 220, draw: 2250, hold: 420, move: 1100 };
  const length = guide.getTotalLength();
  const count = 720;
  const samples = [];
  let frame = 0;
  let started = 0;
  let seen = false;
  let lastSample = -1;

  for (let i = 0; i <= count; i++) {
    const progress = i / count;
    const distance = progress * length;
    const point = guide.getPointAtLength(distance);
    const before = guide.getPointAtLength(Math.max(0, distance - .6));
    const after = guide.getPointAtLength(Math.min(length, distance + .6));
    const dx = after.x - before.x, dy = after.y - before.y;
    const magnitude = Math.hypot(dx, dy) || 1;
    const nx = -dy / magnitude, ny = dx / magnitude;
    // A fixed, angled broad nib gives contrasting hairlines and downstrokes.
    const taper = Math.min(1, .12 + progress / .045, .08 + (1 - progress) / .06);
    const width = (3 + 20 * Math.abs(nx * .82 + ny * .57)) * taper;
    samples.push({
      x: point.x, y: point.y,
      left: `${(point.x + nx * width).toFixed(2)},${(point.y + ny * width).toFixed(2)}`,
      right: `${(point.x - nx * width).toFixed(2)},${(point.y - ny * width).toFixed(2)}`,
    });
  }

  function draw(progress) {
    const end = Math.round(progress * count);
    if (end === lastSample) return;
    lastSample = end;
    const visible = samples.slice(0, end + 1);
    ink.setAttribute('d', end ? `M${visible.map(p => p.left).join('L')}L${visible.reverse().map(p => p.right).join('L')}Z` : '');
    tip.setAttribute('cx', samples[end].x);
    tip.setAttribute('cy', samples[end].y);
  }

  function stop() {
    cancelAnimationFrame(frame);
    frame = 0;
  }

  function finish(focus = false) {
    stop();
    draw(1);
    panel.dataset.phase = 'ready';
    card.inert = false;
    card.removeAttribute('aria-hidden');
    skip.hidden = true;
    replay.hidden = motion.matches;
    if (focus && !panel.hidden) document.getElementById('authUsername').focus({ preventScroll: true });
  }

  function tick(now) {
    if (panel.hidden) { stop(); return; }
    if (!started) started = now;
    const elapsed = now - started;
    const progress = Math.max(0, Math.min(1, (elapsed - duration.lead) / duration.draw));
    // Smooth acceleration at the start; a slight slowdown as the pen lifts.
    draw(progress * progress * (3 - 2 * progress));
    if (elapsed >= duration.lead + duration.draw + duration.hold + duration.move) {
      finish(document.activeElement === skip || document.activeElement === document.body);
      return;
    }
    const moving = elapsed >= duration.lead + duration.draw + duration.hold;
    panel.dataset.phase = moving ? 'moving' : progress === 1 ? 'holding' : 'drawing';
    if (moving && card.inert) {
      // Once the form appears it must accept input, even while sliding in.
      card.inert = false;
      card.removeAttribute('aria-hidden');
    }
    frame = requestAnimationFrame(tick);
  }

  function play() {
    stop();
    if (motion.matches) { finish(); return; }
    started = 0;
    draw(0);
    panel.dataset.phase = 'drawing';
    card.inert = true;
    card.setAttribute('aria-hidden', 'true');
    skip.hidden = false;
    replay.hidden = true;
    frame = requestAnimationFrame(tick);
  }

  skip.addEventListener('click', () => finish(true));
  replay.addEventListener('click', () => { play(); if (!motion.matches) skip.focus({ preventScroll: true }); });
  panel.addEventListener('keydown', event => {
    if (event.key === 'Escape' && panel.dataset.phase !== 'ready') {
      event.preventDefault();
      finish(true);
    }
  });
  motion.addEventListener('change', () => {
    if (motion.matches) finish(document.activeElement === skip);
    else replay.hidden = panel.dataset.phase !== 'ready';
  });
  document.addEventListener('visibilitychange', () => {
    if (document.hidden && !panel.hidden) finish();
  });

  window.AuthUI = {
    show({ immediate = false } = {}) {
      if (seen || immediate || motion.matches) finish();
      else play();
      seen = true;
    },
    hide() { stop(); seen = true; },
  };
  draw(1);
})();
