// js/components.js
//
// Interactive component registry for the Weiran Web UI.
//
// Each component renders from a Markdown code fence:   ```weiran-<type>
// To add a new component, call WeiranComponents.register('<type>', spec).
// The marked-renderer dispatch in index.html is generic — it routes any
// `weiran-<type>` fence through WeiranComponents.render, so new components
// don't touch that dispatch.
//
// spec = {
//   render(data)            -> string (HTML)
//   handlers?               -> {name: fn}  exposed on window for inline onclick
//   onUserReply?(el, text)  -> lock container after history-replay
// }
//
// Depends on app.js globals: esc, openLightbox, wsSend, activeSessionId.
// Loaded before app.js's marked dispatch runs (see <script> order in index.html).

(function (global) {
  'use strict';

  const registry = Object.create(null);

  function register(type, spec) {
    registry[type] = spec;
    // Inline onclick="foo(this)" needs global handlers. Expose all declared ones.
    if (spec.handlers) {
      for (const [name, fn] of Object.entries(spec.handlers)) {
        global[name] = fn;
      }
    }
  }

  function render(type, data) {
    const spec = registry[type];
    if (!spec || !spec.render) return '';
    try {
      return spec.render(data);
    } catch (e) {
      console.warn('[component]', type, 'render failed:', e);
      return '';
    }
  }

  // History replay helper: for every registered type with an onUserReply hook,
  // scan `rootEl` for `.<type>-container` elements and let the spec lock them.
  // Replaces the old per-type switch in disableAnsweredInteractives().
  function restoreAnswers(rootEl, replyText) {
    for (const [type, spec] of Object.entries(registry)) {
      if (!spec.onUserReply) continue;
      rootEl.querySelectorAll('.' + type + '-container').forEach(c => {
        try { spec.onUserReply(c, replyText); }
        catch (e) { console.warn('[component]', type, 'onUserReply failed:', e); }
      });
    }
  }

  global.WeiranComponents = { register, render, restoreAnswers, _registry: registry };

  // Convenience accessors for app.js globals (resolved lazily so load order
  // doesn't matter).
  const esc = s => (global.esc ? global.esc(s) : String(s == null ? '' : s));
  const wsSend = msg => global.wsSend && global.wsSend(msg);
  // activeSessionId is declared with `let` in index.html's inline script —
  // classic-script top-level `let` goes into the shared global lexical env,
  // NOT onto window. So we must look it up lexically, not via global.X.
  // (Same gotcha that broke TOKEN lookup in drawer-tmux.js, commit 565ced9.)
  const sid = () => {
    try { return typeof activeSessionId !== 'undefined' ? activeSessionId : null; }
    catch (_) { return null; }
  };

  // Shared ID counter across all components.
  let _idCtr = 0;
  const nextId = prefix => prefix + '-' + (++_idCtr) + '-' + Date.now();

  // ═══════════════════════════════════════════════════════════════════════
  // choices — GAL-style single/multi choice cards
  // ═══════════════════════════════════════════════════════════════════════
  register('choices', {
    render(data) {
      const type = data.type || 'single';
      const id = nextId('wc');
      let html = `<div class="choices-container" data-choices-id="${id}" data-choices-type="${type}">`;
      (data.options || []).forEach((opt, i) => {
        const oid = opt.id || String.fromCharCode(65 + i);
        const imgHtml = opt.img ? `<img class="choice-img" src="${esc(opt.img)}" onclick="event.stopPropagation();openLightbox(this.src)" loading="lazy">` : '';
        const descHtml = opt.desc ? `<div class="choice-desc">${esc(opt.desc)}</div>` : '';
        html += `<div class="choice-card" data-choice-id="${esc(oid)}" data-choice-label="${esc(opt.label)}" onclick="handleChoiceClick(this)">`
          + `<div class="choice-id">${esc(oid)}</div>`
          + `<div class="choice-body"><div class="choice-label">${esc(opt.label)}</div>${descHtml}</div>`
          + imgHtml
          + `</div>`;
      });
      if (type === 'multi') {
        html += `<button class="choices-confirm" disabled onclick="submitMultiChoice(this)">确认选择</button>`;
      }
      html += `</div>`;
      return html;
    },
    handlers: {
      handleChoiceClick(el) {
        const container = el.closest('.choices-container');
        const s = sid();
        if (!container || !s) return;
        const type = container.dataset.choicesType || 'single';
        const id = el.dataset.choiceId;
        const label = el.dataset.choiceLabel;
        if (type === 'single') {
          container.querySelectorAll('.choice-card').forEach(c => { c.classList.add('disabled'); c.classList.remove('selected'); });
          el.classList.add('selected'); el.classList.remove('disabled');
          wsSend({ type: 'send', sid: s, message: `${id}. ${label}` });
        } else {
          el.classList.toggle('selected');
          const btn = container.querySelector('.choices-confirm');
          if (btn) btn.disabled = !container.querySelector('.choice-card.selected');
        }
      },
      submitMultiChoice(btn) {
        const container = btn.closest('.choices-container');
        const s = sid();
        if (!container || !s) return;
        const selected = [];
        container.querySelectorAll('.choice-card.selected').forEach(c => {
          selected.push(`${c.dataset.choiceId}. ${c.dataset.choiceLabel}`);
        });
        if (!selected.length) return;
        container.querySelectorAll('.choice-card').forEach(c => c.classList.add('disabled'));
        btn.disabled = true; btn.textContent = '已提交';
        wsSend({ type: 'send', sid: s, message: selected.join(', ') });
      },
    },
    onUserReply(c, replyText) {
      c.querySelectorAll('.choice-card').forEach(card => {
        const id = card.dataset.choiceId;
        const label = card.dataset.choiceLabel;
        if (replyText.includes(`${id}. ${label}`) || replyText === `${id}. ${label}`) {
          card.classList.add('selected');
        }
        card.classList.add('disabled');
      });
      const btn = c.querySelector('.choices-confirm');
      if (btn) { btn.disabled = true; btn.textContent = '已提交'; }
    },
  });

  // ═══════════════════════════════════════════════════════════════════════
  // chips — Quick reply bubbles
  // ═══════════════════════════════════════════════════════════════════════
  register('chips', {
    render(data) {
      const id = nextId('wchip');
      let html = `<div class="chips-container" data-chips-id="${id}">`;
      (data.options || []).forEach(opt => {
        const label = typeof opt === 'string' ? opt : opt.label;
        const value = typeof opt === 'string' ? opt : (opt.value || opt.label);
        html += `<button class="chip-btn" data-chip-value="${esc(value)}" onclick="handleChipClick(this)">${esc(label)}</button>`;
      });
      html += `</div>`;
      return html;
    },
    handlers: {
      handleChipClick(el) {
        const container = el.closest('.chips-container');
        const s = sid();
        if (!container || !s) return;
        container.querySelectorAll('.chip-btn').forEach(c => c.classList.add('disabled'));
        el.classList.add('selected'); el.classList.remove('disabled');
        wsSend({ type: 'send', sid: s, message: el.dataset.chipValue });
      },
    },
    onUserReply(c, replyText) {
      c.querySelectorAll('.chip-btn').forEach(chip => {
        if (chip.dataset.chipValue === replyText) chip.classList.add('selected');
        chip.classList.add('disabled');
      });
    },
  });

  // ═══════════════════════════════════════════════════════════════════════
  // rating — Star rating widget
  // ═══════════════════════════════════════════════════════════════════════
  register('rating', {
    render(data) {
      const id = nextId('wrate');
      const max = data.max || 5;
      const target = data.target || '';
      const label = data.label || '';
      let html = `<div class="rating-container" data-rating-id="${id}" data-rating-target="${esc(target)}">`;
      if (label) html += `<span class="rating-label">${esc(label)}</span>`;
      html += `<div class="rating-stars" onmouseleave="ratingPreviewClear(this)">`;
      for (let i = 1; i <= max; i++) {
        html += `<span class="rating-star" data-star="${i}" onclick="handleRatingClick(this,${i})" onmouseenter="ratingPreview(this,${i})">⭐</span>`;
      }
      html += `</div></div>`;
      return html;
    },
    handlers: {
      ratingPreview(el, n) {
        const stars = el.closest('.rating-stars');
        if (stars.classList.contains('disabled')) return;
        stars.querySelectorAll('.rating-star').forEach((s, i) => {
          s.classList.toggle('hover-preview', i < n);
        });
      },
      ratingPreviewClear(container) {
        if (container.classList.contains('disabled')) return;
        container.querySelectorAll('.rating-star:not(.active)').forEach(s => s.classList.remove('hover-preview'));
      },
      handleRatingClick(el, n) {
        const container = el.closest('.rating-container');
        const stars = el.closest('.rating-stars');
        const s = sid();
        if (!container || !s || stars.classList.contains('disabled')) return;
        const target = container.dataset.ratingTarget;
        stars.querySelectorAll('.rating-star').forEach((st, i) => {
          st.classList.toggle('active', i < n);
          st.classList.remove('hover-preview');
        });
        stars.classList.add('disabled');
        const msg = target ? `⭐ ${n} — ${target}` : `⭐ ${n}`;
        wsSend({ type: 'send', sid: s, message: msg });
      },
    },
    onUserReply(c, replyText) {
      const m = replyText.match(/⭐\s*(\d+)/);
      if (!m) return;
      const n = parseInt(m[1]);
      const stars = c.querySelector('.rating-stars');
      if (stars) {
        stars.querySelectorAll('.rating-star').forEach((s, i) => { s.classList.toggle('active', i < n); });
        stars.classList.add('disabled');
      }
    },
  });

  // ═══════════════════════════════════════════════════════════════════════
  // gallery — Horizontally-scrollable image gallery, optionally single-select
  // ═══════════════════════════════════════════════════════════════════════
  register('gallery', {
    render(data) {
      const id = nextId('wgal');
      const selectable = data.selectable !== false;
      let html = `<div class="gallery-container" data-gallery-id="${id}" data-gallery-selectable="${selectable}">`;
      html += `<div class="gallery-scroll">`;
      (data.images || []).forEach((img, i) => {
        const url = typeof img === 'string' ? img : img.url;
        const caption = typeof img === 'object' ? img.caption : '';
        const gid = typeof img === 'object' && img.id ? img.id : (caption || `#${i + 1}`);
        html += `<div class="gallery-item" data-gallery-gid="${esc(gid)}" onclick="handleGalleryClick(this)">`
          + `<img src="${esc(url)}" loading="lazy">`
          + (caption ? `<div class="gallery-caption">${esc(caption)}</div>` : '')
          + `<div class="gallery-check">✓</div>`
          + `<div class="gallery-zoom" onclick="event.stopPropagation();openLightbox(this.closest('.gallery-item').querySelector('img').src)">🔍</div>`
          + `</div>`;
      });
      html += `</div>`;
      if (selectable) html += `<button class="choices-confirm" disabled onclick="submitGalleryChoice(this)">选这个</button>`;
      html += `</div>`;
      return html;
    },
    handlers: {
      handleGalleryClick(el) {
        const container = el.closest('.gallery-container');
        const s = sid();
        if (!container || !s) return;
        if (container.dataset.gallerySelectable === 'false') return;
        container.querySelectorAll('.gallery-item').forEach(g => g.classList.remove('selected'));
        el.classList.add('selected');
        const btn = container.querySelector('.choices-confirm');
        if (btn) btn.disabled = false;
      },
      submitGalleryChoice(btn) {
        const container = btn.closest('.gallery-container');
        const s = sid();
        if (!container || !s) return;
        const sel = container.querySelector('.gallery-item.selected');
        if (!sel) return;
        container.querySelectorAll('.gallery-item').forEach(g => g.classList.add('disabled'));
        btn.disabled = true; btn.textContent = '已选择';
        wsSend({ type: 'send', sid: s, message: sel.dataset.galleryGid });
      },
    },
    onUserReply(c, replyText) {
      c.querySelectorAll('.gallery-item').forEach(g => {
        if (g.dataset.galleryGid === replyText) g.classList.add('selected');
        g.classList.add('disabled');
      });
      const btn = c.querySelector('.choices-confirm');
      if (btn) { btn.disabled = true; btn.textContent = '已选择'; }
    },
  });

})(window);
