// Board behavior: drag and drop between and within columns, a slide-over task
// detail, filtering, and toasts. Every action here has a plain HTML fallback,
// so the board still works with this script switched off.
(() => {
  'use strict';

  const csrf = document.body.dataset.csrf || '';
  const board = document.getElementById('kanban-board');
  const drawer = document.getElementById('task-drawer');
  const drawerBody = document.getElementById('drawer-body');
  const toastHost = document.getElementById('toasts');
  const filterInput = document.getElementById('board-filter');

  let dragged = null;
  let origin = null;
  let highlighted = null;
  let lastCard = null;

  /* ------------------------------------------------------------- toasts - */

  function toast(message, options = {}) {
    if (!toastHost || !message) return;
    const node = document.createElement('div');
    node.className = options.tone ? `toast ${options.tone}` : 'toast';
    const text = document.createElement('span');
    text.textContent = message;
    node.append(text);

    let timer = 0;
    const dismiss = () => {
      window.clearTimeout(timer);
      node.classList.add('leaving');
      window.setTimeout(() => node.remove(), 220);
    };
    if (options.action) {
      const button = document.createElement('button');
      button.type = 'button';
      button.textContent = options.action.label;
      button.addEventListener('click', () => {
        dismiss();
        options.action.run();
      });
      node.append(button);
    }
    toastHost.append(node);
    timer = window.setTimeout(dismiss, options.action ? 7000 : 3200);
  }

  /* --------------------------------------------------------- board state - */

  const cardsIn = (list) => Array.from(list.querySelectorAll('.task-card'));

  function findCard(id) {
    return board ? board.querySelector(`.task-card[data-task-id="${CSS.escape(id)}"]`) : null;
  }

  function listFor(status) {
    return board ? board.querySelector(`[data-dropzone][data-status="${CSS.escape(status)}"]`) : null;
  }

  // Cards sit above the column's empty-state message, so every insertion is
  // relative to a following node rather than an append.
  function insertCard(list, card, before) {
    const anchor = before && before.parentElement === list ? before : list.querySelector('.column-empty');
    list.insertBefore(card, anchor);
  }

  function placeInColumn(card, status, index) {
    const list = listFor(status);
    if (!list) return;
    const others = cardsIn(list).filter((item) => item !== card);
    insertCard(list, card, others[index] || null);
  }

  function rememberPlace(card) {
    const list = card.parentElement;
    return { status: list.dataset.status, index: cardsIn(list).indexOf(card) };
  }

  function refresh() {
    if (!board) return;
    applyFilter();
    board.querySelectorAll('.kanban-column').forEach((column) => {
      const list = column.querySelector('[data-dropzone]');
      if (!list) return;
      const visible = cardsIn(list).filter((card) => !card.hidden);
      const count = column.querySelector('.column-count');
      if (count) count.textContent = String(visible.length);
      const empty = list.querySelector('.column-empty');
      if (empty) empty.hidden = visible.length > 0;
    });
  }

  function applyFilter() {
    const query = (filterInput ? filterInput.value : '').trim().toLowerCase();
    document.querySelectorAll('.task-card').forEach((card) => {
      if (!query) {
        card.hidden = false;
        return;
      }
      const title = card.querySelector('.card-title');
      const excerpt = card.querySelector('.card-excerpt');
      const haystack = [
        title ? title.textContent : '',
        excerpt ? excerpt.textContent : '',
        card.dataset.taskId || '',
      ].join(' ').toLowerCase();
      card.hidden = !haystack.includes(query);
    });
  }

  /* --------------------------------------------------------------- moves - */

  async function requestMove(id, status, index) {
    const response = await fetch(`/tasks/${encodeURIComponent(id)}/move`, {
      method: 'POST',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded',
      },
      body: new URLSearchParams({ csrf, status, index: String(index) }),
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.error || 'That move did not stick.');
    return payload;
  }

  // swap replaces an optimistically moved card with the server's own markup, so
  // status colors, chips, and move options never drift from the stored task.
  function swap(card, html) {
    const holder = document.createElement('div');
    holder.innerHTML = (html || '').trim();
    const fresh = holder.firstElementChild;
    if (!fresh) {
      card.classList.remove('pending');
      return card;
    }
    card.replaceWith(fresh);
    return fresh;
  }

  function replaceDetail(detail, html) {
    const holder = document.createElement('div');
    holder.innerHTML = (html || '').trim();
    const fresh = holder.firstElementChild;
    if (!fresh) return detail;
    detail.replaceWith(fresh);
    return fresh;
  }

  async function commitMove(card, from) {
    const id = card.dataset.taskId;
    const list = card.parentElement;
    const status = list.dataset.status;
    const index = cardsIn(list).indexOf(card);
    refresh();
    if (from && from.status === status && from.index === index) return card;

    card.classList.add('pending');
    try {
      const result = await requestMove(id, status, index);
      const fresh = swap(card, result.card);
      fresh.classList.add('settled');
      if (from && from.status !== status) {
        toast(result.message, {
          action: { label: 'Undo', run: () => undoMove(id, from) },
        });
      }
      refresh();
      return fresh;
    } catch (error) {
      card.classList.remove('pending');
      if (from) placeInColumn(card, from.status, from.index);
      toast(error.message, { tone: 'error' });
      refresh();
      return card;
    }
  }

  /* ------------------------------------------------- delete and restore - */

  async function requestTaskAction(id, action, fallbackError) {
    const response = await fetch(`/tasks/${encodeURIComponent(id)}/${action}`, {
      method: 'POST',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/x-www-form-urlencoded',
      },
      body: new URLSearchParams({ csrf }),
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.error || fallbackError);
    return payload;
  }

  // Deleting is reversible, so the card leaves the board with an undo attached
  // rather than a confirmation in front of it.
  async function deleteTask(id) {
    try {
      const result = await requestTaskAction(id, 'delete', 'Could not delete that task.');
      const card = findCard(id);
      if (card) card.remove();
      closeDrawer();
      refresh();
      toast(result.message, { action: { label: 'Undo', run: () => undoDelete(id) } });
    } catch (error) {
      toast(error.message, { tone: 'error' });
    }
  }

  async function undoDelete(id) {
    try {
      const result = await requestTaskAction(id, 'restore', 'Could not restore that task.');
      const list = listFor(result.status);
      const holder = document.createElement('div');
      holder.innerHTML = (result.card || '').trim();
      const fresh = holder.firstElementChild;
      if (list && fresh) {
        insertCard(list, fresh, cardsIn(list)[result.index] || null);
        fresh.classList.add('settled');
      }
      refresh();
    } catch (error) {
      toast(error.message, { tone: 'error' });
    }
  }

  async function undoMove(id, place) {
    const card = findCard(id);
    if (!card) return;
    card.classList.add('pending');
    try {
      const result = await requestMove(id, place.status, place.index);
      const fresh = swap(card, result.card);
      placeInColumn(fresh, place.status, place.index);
      fresh.classList.add('settled');
    } catch (error) {
      card.classList.remove('pending');
      toast(error.message, { tone: 'error' });
    }
    refresh();
  }

  /* ----------------------------------------------------- drag and drop - */

  function highlight(list) {
    if (highlighted === list) return;
    clearHighlight();
    list.classList.add('drop-active');
    highlighted = list;
  }

  function clearHighlight() {
    if (highlighted) highlighted.classList.remove('drop-active');
    highlighted = null;
  }

  function cardAfterPointer(list, y) {
    return cardsIn(list)
      .filter((card) => card !== dragged)
      .find((card) => {
        const box = card.getBoundingClientRect();
        return y < box.top + box.height / 2;
      }) || null;
  }

  if (board) {
    board.addEventListener('dragstart', (event) => {
      const card = event.target.closest('.task-card');
      if (!card) return;
      dragged = card;
      origin = rememberPlace(card);
      event.dataTransfer.effectAllowed = 'move';
      event.dataTransfer.setData('text/plain', card.dataset.taskId);
      // Deferring the class keeps the browser's drag image opaque.
      window.requestAnimationFrame(() => card.classList.add('dragging'));
    });

    board.addEventListener('dragover', (event) => {
      if (!dragged) return;
      const list = event.target.closest('[data-dropzone]');
      if (!list) return;
      event.preventDefault();
      event.dataTransfer.dropEffect = 'move';
      highlight(list);
      insertCard(list, dragged, cardAfterPointer(list, event.clientY));
    });

    board.addEventListener('drop', (event) => {
      if (!dragged) return;
      event.preventDefault();
      const card = dragged;
      const from = origin;
      dragged = null;
      origin = null;
      card.classList.remove('dragging');
      clearHighlight();
      commitMove(card, from);
    });

    // A drag released outside a column leaves the card where it started.
    board.addEventListener('dragend', () => {
      if (!dragged) return;
      dragged.classList.remove('dragging');
      if (origin) placeInColumn(dragged, origin.status, origin.index);
      dragged = null;
      origin = null;
      clearHighlight();
      refresh();
    });

    board.addEventListener('dragleave', (event) => {
      if (dragged && !event.relatedTarget) clearHighlight();
    });

    // The per-card move menu posts the same form the server handles without
    // JavaScript; intercepting it keeps the board in place.
    board.addEventListener('submit', (event) => {
      const form = event.target;
      if (!form.closest('.card-move')) return;
      const status = event.submitter && event.submitter.value;
      if (!status) return;
      event.preventDefault();
      const card = form.closest('.task-card');
      const menu = form.closest('details');
      if (menu) menu.open = false;
      const from = rememberPlace(card);
      placeInColumn(card, status, 0);
      commitMove(card, from);
    });

    board.addEventListener('keydown', (event) => {
      const card = event.target.closest('.task-card');
      if (!card || event.target !== card) return;
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        lastCard = card;
        openTask(card.dataset.taskId);
        return;
      }
      if (!event.metaKey && !event.ctrlKey) return;
      const lists = Array.from(board.querySelectorAll('[data-dropzone]'));
      const list = card.parentElement;
      const column = lists.indexOf(list);
      const index = cardsIn(list).indexOf(card);
      let target = list;
      let position = index;
      switch (event.key) {
        case 'ArrowLeft':
          target = lists[column - 1] || list;
          position = 0;
          break;
        case 'ArrowRight':
          target = lists[column + 1] || list;
          position = 0;
          break;
        case 'ArrowUp':
          position = Math.max(0, index - 1);
          break;
        case 'ArrowDown':
          position = index + 1;
          break;
        default:
          return;
      }
      if (target === list && position === index) return;
      event.preventDefault();
      const from = rememberPlace(card);
      placeInColumn(card, target.dataset.status, position);
      commitMove(card, from).then((fresh) => fresh && fresh.focus());
    });
  }

  /* --------------------------------------------------------------- detail - */

  async function openTask(id, options = {}) {
    if (!drawer || !drawerBody) return;
    try {
      const response = await fetch(`/${encodeURIComponent(id)}?partial=1`, {
        headers: { 'Accept': 'text/html' },
      });
      if (!response.ok) throw new Error('Could not open that task.');
      drawerBody.innerHTML = await response.text();
    } catch (error) {
      toast(error.message, { tone: 'error' });
      return;
    }
    drawer.hidden = false;
    document.body.classList.add('drawer-open');
    if (options.push !== false) history.pushState({ taskId: id }, '', `/${id}`);
    const panel = drawer.querySelector('.drawer-panel');
    if (panel) panel.focus();
  }

  async function reloadDrawer(id) {
    if (!drawer || drawer.hidden) return;
    const response = await fetch(`/${encodeURIComponent(id)}?partial=1`, {
      headers: { 'Accept': 'text/html' },
    });
    if (response.ok) drawerBody.innerHTML = await response.text();
  }

  function closeDrawer(options = {}) {
    if (!drawer || drawer.hidden) return;
    drawer.hidden = true;
    drawerBody.innerHTML = '';
    document.body.classList.remove('drawer-open');
    if (options.restoreHistory !== false) {
      if (history.state && history.state.taskId) history.back();
      else history.replaceState({}, '', '/');
    }
    if (lastCard && lastCard.isConnected) lastCard.focus();
  }

  if (board) {
    board.addEventListener('click', (event) => {
      if (event.defaultPrevented || event.button !== 0) return;
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      const card = event.target.closest('.task-card');
      if (!card || event.target.closest('form, details, button')) return;
      event.preventDefault();
      lastCard = card;
      openTask(card.dataset.taskId);
    });
  }

  if (drawer) {
    drawer.addEventListener('click', (event) => {
      if (event.target.closest('[data-drawer-close]')) closeDrawer();
    });

    // Working inside the drawer updates both the panel and the card behind it.
    drawerBody.addEventListener('submit', async (event) => {
      const form = event.target;
      if (!board) return;
      const detail = form.closest('.task-detail');
      if (!detail) return;
      const id = detail.dataset.taskId;

      if (form.classList.contains('upload')) {
        event.preventDefault();
        try {
          const response = await fetch(form.action, {
            method: 'POST',
            headers: { 'Accept': 'application/json' },
            body: new FormData(form),
          });
          const payload = await response.json().catch(() => ({}));
          if (!response.ok) throw new Error(payload.error || 'Could not upload that file.');
          const card = findCard(id);
          if (card) swap(card, payload.card).classList.add('settled');
          await reloadDrawer(id);
          refresh();
          toast(payload.message);
        } catch (error) {
          toast(error.message, { tone: 'error' });
        }
        return;
      }

      if (form.classList.contains('detail-delete')) {
        event.preventDefault();
        deleteTask(id);
        return;
      }

      if (!form.classList.contains('detail-moves')) return;
      const status = event.submitter && event.submitter.value;
      if (!status) return;
      event.preventDefault();
      try {
        const result = await requestMove(id, status, 0);
        const card = findCard(id);
        if (card) {
          const fresh = swap(card, result.card);
          placeInColumn(fresh, result.status, 0);
          fresh.classList.add('settled');
        }
        refresh();
        await reloadDrawer(id);
        toast(result.message);
      } catch (error) {
        toast(error.message, { tone: 'error' });
      }
    });
  }

  /* --------------------------------------------------------------- edits - */

  function beginEdit(field) {
    if (!field || field.classList.contains('editing')) return;
    const display = field.querySelector('[data-edit-display]');
    const form = field.querySelector('.inline-edit');
    const control = form && form.elements.value;
    if (!display || !form || !control) return;
    field.classList.add('editing');
    display.hidden = true;
    form.hidden = false;
    // The rich editor is a progressive enhancement over the plain control. When
    // it mounts, it takes focus; otherwise the raw input/textarea is the editor.
    const editor = mountEditor(form, field, display);
    if (editor) {
      focusEditorEnd(editor);
      return;
    }
    control.focus();
    if (control instanceof HTMLInputElement) control.select();
    else control.setSelectionRange(control.value.length, control.value.length);
  }

  function cancelEdit(field) {
    if (!field) return;
    const display = field.querySelector('[data-edit-display]');
    const form = field.querySelector('.inline-edit');
    if (!display || !form) return;
    unmountEditor(form);
    field.classList.remove('editing');
    form.hidden = true;
    display.hidden = false;
    display.focus();
  }

  document.addEventListener('dblclick', (event) => {
    const display = event.target.closest('[data-edit-display]');
    if (!display) return;
    event.preventDefault();
    beginEdit(display.closest('[data-edit-field]'));
  });

  document.addEventListener('click', (event) => {
    const cancel = event.target.closest('[data-edit-cancel]');
    if (!cancel) return;
    cancelEdit(cancel.closest('[data-edit-field]'));
  });

  document.addEventListener('keydown', (event) => {
    const display = event.target.closest('[data-edit-display]');
    if (display && event.target === display && (event.key === 'Enter' || event.key === ' ')) {
      event.preventDefault();
      beginEdit(display.closest('[data-edit-field]'));
      return;
    }

    const form = event.target.closest('.inline-edit');
    if (!form) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      cancelEdit(form.closest('[data-edit-field]'));
      return;
    }
    const field = form.closest('[data-edit-field]');
    const saveTitle = field && field.dataset.editField === 'title' && event.key === 'Enter';
    const saveDescription = field && field.dataset.editField === 'description' &&
      event.key === 'Enter' && (event.metaKey || event.ctrlKey);
    if (saveTitle || saveDescription) {
      event.preventDefault();
      form.requestSubmit();
    }
  });

  document.addEventListener('submit', async (event) => {
    const form = event.target.closest('.inline-edit');
    if (!form) return;
    event.preventDefault();
    // Fold the rich editor's content back into the plain control the form
    // posts, so the whole existing edit pipeline runs unchanged.
    syncEditor(form);
    const detail = form.closest('.task-detail');
    const fieldName = form.elements.field.value;
    const submit = form.querySelector('[type="submit"]');
    form.setAttribute('aria-busy', 'true');
    if (submit) submit.disabled = true;
    try {
      const response = await fetch(form.action, {
        method: 'POST',
        headers: {
          'Accept': 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded',
        },
        body: new URLSearchParams(new FormData(form)),
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(payload.error || 'Could not save that edit.');

      const card = findCard(payload.id);
      if (card) swap(card, payload.card).classList.add('settled');
      const freshDetail = replaceDetail(detail, payload.detail);
      const freshDisplay = freshDetail.querySelector(
        `[data-edit-field="${CSS.escape(fieldName)}"] [data-edit-display]`,
      );
      if (freshDisplay) freshDisplay.focus();
      if (!board) document.title = `${payload.title} · Tasks`;
      refresh();
      toast(payload.message);
    } catch (error) {
      toast(error.message, { tone: 'error' });
    } finally {
      form.removeAttribute('aria-busy');
      if (submit) submit.disabled = false;
    }
  });

  document.addEventListener('keydown', (event) => {
    if (!event.defaultPrevented && event.key === 'Escape' && drawer && !drawer.hidden) {
      event.preventDefault();
      closeDrawer();
    }
  });

  window.addEventListener('popstate', (event) => {
    const id = event.state && event.state.taskId;
    if (id) openTask(id, { push: false });
    else closeDrawer({ restoreHistory: false });
  });

  /* --------------------------------------------------------------- filter - */

  if (filterInput) {
    filterInput.addEventListener('input', refresh);
    filterInput.addEventListener('keydown', (event) => {
      if (event.key !== 'Escape') return;
      filterInput.value = '';
      refresh();
    });
  }

  /* ----------------------------------------------------------- rich editor - */
  //
  // A What-You-See-Is-What-You-Get Markdown editor that enhances the plain
  // title input and description textarea. It is a contenteditable surface seeded
  // from the server's own rendered HTML, so the editing view matches the reading
  // view without a client-side Markdown parser. On save it serializes back to
  // Markdown and hands the text to the plain control the form already posts, so
  // the whole edit pipeline — version checks, optimistic card refresh, toasts —
  // runs unchanged. With scripting off, the raw input and textarea remain.

  const BLOCK_TAGS = new Set([
    'P', 'DIV', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6',
    'UL', 'OL', 'LI', 'BLOCKQUOTE', 'PRE', 'TABLE', 'HR', 'FIGURE',
  ]);

  const TITLE_TOOLS = ['bold', 'italic', 'strike', 'code'];
  const DESC_TOOLS = [
    'bold', 'italic', 'strike', 'code', '|',
    'h2', 'h3', 'quote', 'ul', 'ol', 'task', '|',
    'link', 'codeblock',
  ];
  const TOOL_META = {
    bold: { label: 'B', title: 'Bold (⌘/Ctrl+B)', className: 'is-bold' },
    italic: { label: 'I', title: 'Italic (⌘/Ctrl+I)', className: 'is-italic' },
    strike: { label: 'S', title: 'Strikethrough', className: 'is-strike' },
    code: { label: '<>', title: 'Inline code (⌘/Ctrl+E)' },
    h2: { label: 'H2', title: 'Heading' },
    h3: { label: 'H3', title: 'Subheading' },
    quote: { label: '”', title: 'Quote' },
    ul: { label: '•', title: 'Bulleted list' },
    ol: { label: '1.', title: 'Numbered list' },
    task: { label: '☑', title: 'Task list' },
    link: { label: '🔗', title: 'Link (⌘/Ctrl+K)' },
    codeblock: { label: '{ }', title: 'Code block' },
  };

  let activeEditor = null;

  function supportsRichEditor() {
    return (
      typeof document.execCommand === 'function' &&
      'contentEditable' in document.documentElement
    );
  }

  function mountEditor(form, field, display) {
    if (!supportsRichEditor()) return null;
    if (form._wysiwyg) return form._wysiwyg.editor;
    const fieldName = field.dataset.editField;
    if (fieldName !== 'title' && fieldName !== 'description') return null;
    const control = form.elements.value;
    if (!control) return null;

    const editor = document.createElement('div');
    editor.contentEditable = 'true';
    editor.spellcheck = true;
    editor.setAttribute('role', 'textbox');
    editor.setAttribute('aria-label', fieldName === 'title' ? 'Task title' : 'Description');
    editor.className = fieldName === 'title'
      ? 'wysiwyg-surface wysiwyg-title'
      : 'wysiwyg-surface wysiwyg-body description';
    if (fieldName === 'description') editor.setAttribute('aria-multiline', 'true');

    if (fieldName === 'description') editor.dataset.placeholder = 'Write a description…';

    const source = fieldName === 'title' ? display : display.querySelector('.description');
    editor.innerHTML = source ? source.innerHTML : '';
    prepareEditor(editor);

    const toolbar = buildToolbar(fieldName, editor);
    const wrapper = document.createElement('div');
    wrapper.className = 'wysiwyg';
    wrapper.append(toolbar, editor);
    control.hidden = true;
    form.insertBefore(wrapper, control);
    form._wysiwyg = { editor, toolbar, wrapper, fieldName };
    activeEditor = editor;

    try { document.execCommand('defaultParagraphSeparator', false, 'p'); } catch (_) { /* older browsers */ }
    try { document.execCommand('styleWithCSS', false, false); } catch (_) { /* older browsers */ }
    attachEditorEvents(editor, fieldName);
    syncToolbarState(editor);
    return editor;
  }

  function unmountEditor(form) {
    if (!form || !form._wysiwyg) return;
    const { editor, wrapper } = form._wysiwyg;
    if (activeEditor === editor) activeEditor = null;
    wrapper.remove();
    const control = form.elements.value;
    if (control) control.hidden = false;
    delete form._wysiwyg;
  }

  function syncEditor(form) {
    if (!form || !form._wysiwyg) return;
    const { editor, fieldName } = form._wysiwyg;
    const control = form.elements.value;
    if (!control) return;
    control.value = fieldName === 'title'
      ? serializeInline(editor)
      : serializeMarkdown(editor);
  }

  // prepareEditor makes the seeded HTML editable: task-list checkboxes render
  // disabled for reading, so re-enable them to toggle while editing.
  function prepareEditor(editor) {
    editor.querySelectorAll('input[type="checkbox"]').forEach((box) => {
      box.removeAttribute('disabled');
    });
  }

  function focusEditorEnd(editor) {
    editor.focus();
    const selection = window.getSelection();
    if (!selection) return;
    const range = document.createRange();
    range.selectNodeContents(editor);
    range.collapse(false);
    selection.removeAllRanges();
    selection.addRange(range);
  }

  function buildToolbar(fieldName, editor) {
    const toolbar = document.createElement('div');
    toolbar.className = 'wysiwyg-toolbar';
    toolbar.setAttribute('role', 'toolbar');
    toolbar.setAttribute('aria-label', 'Formatting');
    (fieldName === 'title' ? TITLE_TOOLS : DESC_TOOLS).forEach((key) => {
      if (key === '|') {
        const separator = document.createElement('span');
        separator.className = 'wysiwyg-sep';
        separator.setAttribute('aria-hidden', 'true');
        toolbar.append(separator);
        return;
      }
      const meta = TOOL_META[key];
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'wysiwyg-tool' + (meta.className ? ' ' + meta.className : '');
      button.dataset.cmd = key;
      button.textContent = meta.label;
      button.title = meta.title;
      button.setAttribute('aria-label', meta.title);
      button.setAttribute('aria-pressed', 'false');
      toolbar.append(button);
    });
    // Keep the selection in the editor: a mousedown on a tool must not move
    // focus to the button, or the command would have nothing to act on.
    toolbar.addEventListener('mousedown', (event) => {
      if (event.target.closest('.wysiwyg-tool')) event.preventDefault();
    });
    toolbar.addEventListener('click', (event) => {
      const button = event.target.closest('.wysiwyg-tool');
      if (!button) return;
      event.preventDefault();
      execTool(button.dataset.cmd, editor);
    });
    return toolbar;
  }

  function attachEditorEvents(editor, fieldName) {
    editor.addEventListener('keydown', (event) => {
      const meta = event.metaKey || event.ctrlKey;
      if (meta && !event.altKey) {
        const key = event.key.toLowerCase();
        if (key === 'b') { event.preventDefault(); execTool('bold', editor); return; }
        if (key === 'i') { event.preventDefault(); execTool('italic', editor); return; }
        if (key === 'e') { event.preventDefault(); execTool('code', editor); return; }
        if (fieldName === 'description' && key === 'k') {
          event.preventDefault();
          execTool('link', editor);
          return;
        }
      }
      // Enter submits a single-line title; the shared .inline-edit handler does
      // the submit, this just stops a newline from being inserted first.
      if (fieldName === 'title' && event.key === 'Enter') event.preventDefault();
    });
    editor.addEventListener('input', () => syncToolbarState(editor));
    editor.addEventListener('keyup', () => syncToolbarState(editor));
    editor.addEventListener('mouseup', () => syncToolbarState(editor));
    // Checkboxes inside contenteditable do not always toggle on their own.
    editor.addEventListener('click', (event) => {
      const box = event.target.closest('input[type="checkbox"]');
      if (!box) return;
      window.requestAnimationFrame(() => box.toggleAttribute('checked', box.checked));
    });
  }

  /* ------------------------------------------------------- editor commands - */

  function execTool(key, editor) {
    editor.focus();
    switch (key) {
      case 'bold': document.execCommand('bold'); break;
      case 'italic': document.execCommand('italic'); break;
      case 'strike': document.execCommand('strikeThrough'); break;
      case 'ul': document.execCommand('insertUnorderedList'); break;
      case 'ol': document.execCommand('insertOrderedList'); break;
      case 'h2': toggleBlock(editor, 'h2'); break;
      case 'h3': toggleBlock(editor, 'h3'); break;
      case 'quote': toggleBlock(editor, 'blockquote'); break;
      case 'task': toggleTaskList(editor); break;
      case 'code': toggleInlineCode(editor); break;
      case 'codeblock': toggleCodeBlock(editor); break;
      case 'link': insertLink(editor); break;
      default: break;
    }
    syncToolbarState(editor);
  }

  function toggleBlock(editor, tag) {
    const block = currentBlock(editor);
    const target = block && block.tagName === tag.toUpperCase() ? 'p' : tag;
    document.execCommand('formatBlock', false, target);
  }

  function currentBlock(editor) {
    const selection = window.getSelection();
    if (!selection || !selection.rangeCount) return null;
    let node = selection.getRangeAt(0).startContainer;
    if (node.nodeType === Node.TEXT_NODE) node = node.parentNode;
    while (node && node !== editor) {
      if (node.nodeType === Node.ELEMENT_NODE && BLOCK_TAGS.has(node.tagName)) return node;
      node = node.parentNode;
    }
    return null;
  }

  function currentListItem(editor) {
    const selection = window.getSelection();
    if (!selection || !selection.rangeCount) return null;
    let node = selection.getRangeAt(0).startContainer;
    if (node.nodeType === Node.TEXT_NODE) node = node.parentNode;
    while (node && node !== editor) {
      if (node.nodeType === Node.ELEMENT_NODE && node.tagName === 'LI') return node;
      node = node.parentNode;
    }
    return null;
  }

  function ancestorMatching(editor, test) {
    const selection = window.getSelection();
    if (!selection || !selection.rangeCount) return null;
    let node = selection.getRangeAt(0).startContainer;
    if (node.nodeType === Node.TEXT_NODE) node = node.parentNode;
    while (node && node !== editor) {
      if (node.nodeType === Node.ELEMENT_NODE && test(node)) return node;
      node = node.parentNode;
    }
    return null;
  }

  function toggleTaskList(editor) {
    let item = currentListItem(editor);
    if (!item) {
      document.execCommand('insertUnorderedList');
      item = currentListItem(editor);
      if (!item) return;
    }
    const existing = item.querySelector(':scope > input[type="checkbox"]');
    if (existing) {
      existing.remove();
      item.classList.remove('task-list-item');
      return;
    }
    const box = document.createElement('input');
    box.type = 'checkbox';
    item.classList.add('task-list-item');
    item.insertBefore(document.createTextNode(' '), item.firstChild);
    item.insertBefore(box, item.firstChild);
  }

  function toggleInlineCode(editor) {
    const selection = window.getSelection();
    if (!selection || !selection.rangeCount) return;
    const existing = ancestorMatching(
      editor,
      (node) => node.tagName === 'CODE' && !node.closest('pre'),
    );
    if (existing) {
      unwrap(existing);
      return;
    }
    const range = selection.getRangeAt(0);
    if (range.collapsed) return;
    const code = document.createElement('code');
    try {
      range.surroundContents(code);
    } catch (_) {
      code.appendChild(range.extractContents());
      range.insertNode(code);
    }
    selectNodeContents(code);
  }

  function toggleCodeBlock(editor) {
    const pre = ancestorMatching(editor, (node) => node.tagName === 'PRE');
    if (pre) {
      const paragraph = document.createElement('p');
      paragraph.textContent = (pre.textContent || '').replace(/\n$/, '');
      if (!paragraph.textContent) paragraph.appendChild(document.createElement('br'));
      pre.replaceWith(paragraph);
      placeCaretAtEnd(paragraph);
      return;
    }
    const block = currentBlock(editor);
    const preEl = document.createElement('pre');
    const code = document.createElement('code');
    code.textContent = block ? block.textContent : '';
    preEl.appendChild(code);
    if (block && block.parentElement === editor) block.replaceWith(preEl);
    else editor.appendChild(preEl);
    placeCaretAtEnd(code);
  }

  function insertLink(editor) {
    const selection = window.getSelection();
    if (!selection || !selection.rangeCount) return;
    const existing = ancestorMatching(editor, (node) => node.tagName === 'A');
    if (existing) {
      unwrap(existing);
      return;
    }
    if (selection.getRangeAt(0).collapsed) return;
    const url = window.prompt('Link URL');
    if (!url) return;
    document.execCommand('createLink', false, url.trim());
  }

  function unwrap(element) {
    const parent = element.parentNode;
    if (!parent) return;
    while (element.firstChild) parent.insertBefore(element.firstChild, element);
    parent.removeChild(element);
  }

  function selectNodeContents(node) {
    const selection = window.getSelection();
    if (!selection) return;
    const range = document.createRange();
    range.selectNodeContents(node);
    selection.removeAllRanges();
    selection.addRange(range);
  }

  function placeCaretAtEnd(node) {
    const selection = window.getSelection();
    if (!selection) return;
    const range = document.createRange();
    range.selectNodeContents(node);
    range.collapse(false);
    selection.removeAllRanges();
    selection.addRange(range);
  }

  function queryState(command) {
    try { return document.queryCommandState(command); } catch (_) { return false; }
  }

  function syncToolbarState(editor) {
    const form = editor.closest('.inline-edit');
    if (!form || !form._wysiwyg) return;
    const states = {
      bold: queryState('bold'),
      italic: queryState('italic'),
      strike: queryState('strikeThrough'),
      h2: !!ancestorMatching(editor, (node) => node.tagName === 'H2'),
      h3: !!ancestorMatching(editor, (node) => node.tagName === 'H3'),
      quote: !!ancestorMatching(editor, (node) => node.tagName === 'BLOCKQUOTE'),
      code: !!ancestorMatching(editor, (node) => node.tagName === 'CODE' && !node.closest('pre')),
      codeblock: !!ancestorMatching(editor, (node) => node.tagName === 'PRE'),
      link: !!ancestorMatching(editor, (node) => node.tagName === 'A'),
      ul: false,
      ol: false,
      task: false,
    };
    const item = currentListItem(editor);
    if (item) {
      const list = item.parentElement;
      states.task = !!item.querySelector(':scope > input[type="checkbox"]');
      if (list && list.tagName === 'OL') states.ol = true;
      else if (list && list.tagName === 'UL' && !states.task) states.ul = true;
    }
    form._wysiwyg.toolbar.querySelectorAll('[data-cmd]').forEach((button) => {
      const active = !!states[button.dataset.cmd];
      button.classList.toggle('active', active);
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
    });
  }

  document.addEventListener('selectionchange', () => {
    if (!activeEditor || !activeEditor.isConnected) return;
    const selection = window.getSelection();
    if (!selection || !selection.rangeCount) return;
    if (activeEditor.contains(selection.getRangeAt(0).startContainer)) {
      syncToolbarState(activeEditor);
    }
  });

  /* ---------------------------------------------------- HTML → Markdown - */

  function serializeMarkdown(root) {
    // Keep trailing spaces: two of them before a newline are a Markdown hard
    // break. Only collapse runs of blank lines.
    const markdown = blockParts(root).join('\n\n');
    return markdown.replace(/\n{3,}/g, '\n\n').trim();
  }

  function serializeInline(root) {
    return inlineChildrenToMd(root).replace(/\s+/g, ' ').trim();
  }

  function blockParts(root) {
    const parts = [];
    root.childNodes.forEach((node) => {
      const markdown = blockToMd(node);
      if (markdown == null) return;
      const trimmed = markdown.replace(/\s+$/, '');
      if (trimmed !== '') parts.push(trimmed);
    });
    return parts;
  }

  function hasBlockChild(node) {
    return Array.from(node.children).some((child) => BLOCK_TAGS.has(child.tagName));
  }

  function blockToMd(node) {
    if (node.nodeType === Node.TEXT_NODE) {
      const text = node.nodeValue.replace(/\s+/g, ' ').trim();
      return text ? escapeBlockLead(escapeInline(text)) : '';
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return '';
    switch (node.tagName) {
      case 'H1': case 'H2': case 'H3': case 'H4': case 'H5': case 'H6':
        return '#'.repeat(Number(node.tagName[1])) + ' ' + inlineChildrenToMd(node).trim();
      case 'P': {
        // A list command can leave a block (a UL/OL) nested inside a paragraph.
        // Serialize those as blocks rather than flattening them into a line.
        if (hasBlockChild(node)) return blockParts(node).join('\n\n');
        const inline = inlineChildrenToMd(node).replace(/^\n+|\n+$/g, '');
        return inline.trim() ? escapeBlockLead(inline) : '';
      }
      case 'UL': return listLines(node, false, '').join('\n');
      case 'OL': return listLines(node, true, '').join('\n');
      case 'BLOCKQUOTE': return blockquoteToMd(node);
      case 'PRE': return preToMd(node);
      case 'HR': return '---';
      case 'TABLE': return tableToMd(node);
      case 'FIGURE': return blockParts(node).join('\n\n');
      case 'BR': return '';
      default: {
        if (hasBlockChild(node)) return blockParts(node).join('\n\n');
        const inline = inlineChildrenToMd(node).trim();
        return inline ? escapeBlockLead(inline) : '';
      }
    }
  }

  function inlineChildrenToMd(element) {
    let out = '';
    element.childNodes.forEach((child) => { out += inlineNodeToMd(child); });
    return out;
  }

  function inlineNodeToMd(node) {
    if (node.nodeType === Node.TEXT_NODE) {
      return escapeInline(node.nodeValue.replace(/\r?\n/g, ' '));
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return '';
    switch (node.tagName) {
      case 'BR': return '  \n';
      case 'STRONG': case 'B': return wrapMarks('**', inlineChildrenToMd(node));
      case 'EM': case 'I': return wrapMarks('*', inlineChildrenToMd(node));
      case 'DEL': case 'S': case 'STRIKE': return wrapMarks('~~', inlineChildrenToMd(node));
      case 'CODE': return codeSpan(node.textContent);
      case 'A': {
        const inner = inlineChildrenToMd(node).trim() || escapeInline(node.textContent.trim());
        const href = node.getAttribute('href') || '';
        return href ? '[' + inner + '](' + href + ')' : inner;
      }
      case 'IMG': {
        const alt = (node.getAttribute('alt') || '').trim();
        const src = node.getAttribute('src') || '';
        return src ? '![' + alt + '](' + src + ')' : '';
      }
      case 'INPUT': return '';
      default: return inlineChildrenToMd(node);
    }
  }

  function wrapMarks(marker, inner) {
    if (!inner) return '';
    const match = inner.match(/^(\s*)([\s\S]*?)(\s*)$/);
    const lead = match[1];
    const core = match[2];
    const trail = match[3];
    if (!core) return inner;
    return lead + marker + core + marker + trail;
  }

  function codeSpan(text) {
    const value = text.replace(/\r?\n/g, ' ');
    let ticks = '`';
    const runs = value.match(/`+/g);
    if (runs) ticks = '`'.repeat(Math.max.apply(null, runs.map((run) => run.length)) + 1);
    const pad = /^`|`$|^\s|\s$/.test(value) && value !== '' ? ' ' : '';
    return ticks + pad + value + pad + ticks;
  }

  function listLines(list, ordered, indent) {
    const lines = [];
    let index = parseInt(list.getAttribute('start') || '1', 10);
    if (!Number.isFinite(index)) index = 1;
    Array.from(list.children).forEach((item) => {
      if (item.tagName !== 'LI') return;
      const task = taskState(item);
      let marker;
      if (task !== null) marker = task ? '- [x] ' : '- [ ] ';
      else if (ordered) { marker = index + '. '; index += 1; }
      else marker = '- ';
      const pad = ' '.repeat(marker.length);

      const split = splitListItem(item);
      const contentLines = split.content.split('\n');
      lines.push(indent + marker + (contentLines[0] || '').replace(/\s+$/, ''));
      for (let i = 1; i < contentLines.length; i += 1) {
        lines.push(indent + pad + contentLines[i]);
      }
      split.sublists.forEach((sub) => {
        listLines(sub, sub.tagName === 'OL', indent + pad).forEach((line) => lines.push(line));
      });
    });
    return lines;
  }

  function taskState(item) {
    const box = item.querySelector(
      ':scope > input[type="checkbox"], :scope > p > input[type="checkbox"], :scope > label > input[type="checkbox"]',
    );
    if (!box) return null;
    return box.checked;
  }

  function splitListItem(item) {
    const contentNodes = [];
    const sublists = [];
    item.childNodes.forEach((node) => {
      if (node.nodeType === Node.ELEMENT_NODE && (node.tagName === 'UL' || node.tagName === 'OL')) {
        sublists.push(node);
      } else {
        contentNodes.push(node);
      }
    });
    let content;
    if (contentNodes.length === 1 &&
        contentNodes[0].nodeType === Node.ELEMENT_NODE &&
        contentNodes[0].tagName === 'P') {
      content = inlineChildrenToMd(contentNodes[0]);
    } else {
      content = contentNodes.map(inlineNodeToMd).join('');
    }
    return { content: content.replace(/^\s+|\s+$/g, ''), sublists };
  }

  function blockquoteToMd(element) {
    const inner = blockParts(element).join('\n\n');
    return inner.split('\n').map((line) => (line ? '> ' + line : '>')).join('\n');
  }

  function preToMd(element) {
    const code = element.querySelector('code') || element;
    let language = '';
    const className = code.getAttribute('class') || element.getAttribute('class') || '';
    const match = className.match(/language-([\w#+.-]+)/);
    if (match) language = match[1];
    const text = (code.textContent || '').replace(/\n$/, '');
    let fence = '```';
    const runs = text.match(/`{3,}/g);
    if (runs) fence = '`'.repeat(Math.max.apply(null, runs.map((run) => run.length)) + 1);
    return fence + language + '\n' + text + '\n' + fence;
  }

  function tableToMd(table) {
    const headRow = table.querySelector('thead tr');
    if (!headRow) return table.textContent.trim();
    const headCells = Array.from(headRow.children);
    const header = headCells.map((cell) => inlineChildrenToMd(cell).trim().replace(/\|/g, '\\|'));
    const aligns = headCells.map((cell) => {
      const align = (cell.getAttribute('align') || cell.style.textAlign || '').toLowerCase();
      if (align === 'center') return ':---:';
      if (align === 'right') return '---:';
      if (align === 'left') return ':---';
      return '---';
    });
    const lines = ['| ' + header.join(' | ') + ' |', '| ' + aligns.join(' | ') + ' |'];
    table.querySelectorAll('tbody tr').forEach((row) => {
      const cells = Array.from(row.children).map(
        (cell) => inlineChildrenToMd(cell).trim().replace(/\|/g, '\\|'),
      );
      lines.push('| ' + cells.join(' | ') + ' |');
    });
    return lines.join('\n');
  }

  function escapeInline(text) {
    return text.replace(/[\\`]/g, '\\$&');
  }

  function escapeBlockLead(text) {
    return text
      .replace(/^(\s*\d+)([.)] )/, '$1\\$2')
      .replace(/^(\s*)([#>+\-*] )/, '$1\\$2')
      .replace(/^(\s*)(#{1,6} )/, '$1\\$2');
  }

  refresh();
})();
