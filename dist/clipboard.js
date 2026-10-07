'use strict';
const AlphaClipboard = (() => {
  function copyWithSelection(text) {
    const active = document.activeElement;
    const selection = document.getSelection();
    const ranges = selection ? Array.from({length: selection.rangeCount}, (_, i) => selection.getRangeAt(i).cloneRange()) : [];
    const inputSelection = active && typeof active.selectionStart === 'number'
      ? [active.selectionStart, active.selectionEnd, active.selectionDirection] : null;
    const field = document.createElement('textarea');
    field.value = text;
    field.readOnly = true;
    field.tabIndex = -1;
    Object.assign(field.style, {position: 'fixed', top: '0', left: '0', width: '1px', height: '1px', padding: '0', border: '0', opacity: '0', fontSize: '16px'});
    // A modal makes the rest of the document inert, so select inside it.
    (active?.closest('dialog[open]') || document.body).append(field);
    try {
      field.focus({preventScroll: true});
      field.select();
      field.setSelectionRange(0, field.value.length);
      if (!document.execCommand('copy')) throw new Error('浏览器未允许复制，请手动复制。');
    } finally {
      field.remove();
      active?.focus({preventScroll: true});
      if (selection) {
        selection.removeAllRanges();
        for (const range of ranges) selection.addRange(range);
      }
      if (inputSelection) active.setSelectionRange(...inputSelection);
    }
  }
  async function writeText(text) {
    if (navigator.clipboard?.writeText) {
      try { await navigator.clipboard.writeText(text); return; }
      catch (_) { /* HTTP access and denied clipboard permissions need selection-based copying. */ }
    }
    copyWithSelection(text);
  }
  return {writeText};
})();
