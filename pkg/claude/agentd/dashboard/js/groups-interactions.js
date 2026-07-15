import { createContext, h } from 'preact';
import { useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);
const GroupsInteractions = createContext(null);

export function GroupsInteractionProvider({ children }) {
  const [openMenuKey, setOpenMenuKey] = useState('');
  const [editorKey, setEditorKey] = useState('');
  const menus = useRef(new Map());
  const editorFocusTarget = useRef(null);
  const pendingEditorFocus = useRef(null);
  const openMenuKeyRef = useRef('');
  openMenuKeyRef.current = openMenuKey;

  useLayoutEffect(() => {
    if (editorKey || !pendingEditorFocus.current) return;
    const { key, target } = pendingEditorFocus.current;
    pendingEditorFocus.current = null;
    // The provider's layout effect can run before its child trigger has
    // committed. Defer from the post-render effect, not from the key handler,
    // so the replacement trigger is queryable in both Preact and real Chrome.
    queueMicrotask(() => {
      if (target?.isConnected) {
        target.focus();
        return;
      }
      [...document.querySelectorAll('[data-editor-key]')]
        .find((node) => node.dataset.editorKey === key)?.focus();
    });
  }, [editorKey]);

  const closeMenu = (restoreFocus = false) => {
    const key = openMenuKeyRef.current;
    if (!key) return;
    const entry = menus.current.get(key);
    const focusInside = !!entry?.menu?.contains(document.activeElement);
    openMenuKeyRef.current = '';
    setOpenMenuKey('');
    if (restoreFocus || focusInside) entry?.button?.focus();
  };

  useEffect(() => {
    const onClick = (event) => {
      const key = openMenuKeyRef.current;
      if (!key) return;
      const entry = menus.current.get(key);
      if (entry?.button?.contains(event.target) || entry?.menu?.contains(event.target)) return;
      closeMenu();
    };
    const onKeyDown = (event) => {
      if (event.key !== 'Escape' || !openMenuKeyRef.current) return;
      event.preventDefault();
      closeMenu(true);
    };
    document.addEventListener('click', onClick);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('click', onClick);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, []);

  const value = useMemo(() => ({
    openMenuKey,
    editorKey,
    registerMenu(key, entry) {
      menus.current.set(key, entry);
      return () => {
        if (menus.current.get(key) === entry) menus.current.delete(key);
      };
    },
    toggleMenu(key) {
      if (openMenuKeyRef.current === key) closeMenu(true);
      else {
        openMenuKeyRef.current = key;
        setOpenMenuKey(key);
      }
    },
    closeMenu,
    beginEditor(key, focusTarget) {
      closeMenu();
      editorFocusTarget.current = focusTarget || document.activeElement;
      setEditorKey(key);
    },
    endEditor(key, restoreFocus = false) {
      setEditorKey((current) => {
        if (current !== key) return current;
        const target = editorFocusTarget.current;
        editorFocusTarget.current = null;
        if (restoreFocus) pendingEditorFocus.current = { key, target };
        return '';
      });
    },
  }), [openMenuKey, editorKey]);

  return html`<${GroupsInteractions.Provider} value=${value}>${children}<//>`;
}

export function useGroupsInteractions() {
  const value = useContext(GroupsInteractions);
  if (!value) throw new Error('Groups interactions require GroupsInteractionProvider');
  return value;
}

export function ActionMenu({ menuKey, kind, wrapperClass, children }) {
  const interactions = useGroupsInteractions();
  const buttonRef = useRef(null);
  const menuRef = useRef(null);
  const [opensUp, setOpensUp] = useState(false);
  const open = interactions.openMenuKey === menuKey;

  useLayoutEffect(() => interactions.registerMenu(menuKey, {
    button: buttonRef.current,
    menu: menuRef.current,
  }), [menuKey]);

  useLayoutEffect(() => {
    const menu = menuRef.current;
    const dismissItem = (event) => {
      if (event.target.closest('button')) interactions.closeMenu();
    };
    menu.addEventListener('click', dismissItem);
    return () => menu.removeEventListener('click', dismissItem);
  }, [menuKey, interactions.closeMenu]);

  useLayoutEffect(() => {
    if (!open) {
      setOpensUp(false);
      return;
    }
    const menu = menuRef.current;
    const button = buttonRef.current;
    if (!menu || !button) return;
    const rect = menu.getBoundingClientRect();
    setOpensUp(rect.bottom > window.innerHeight && rect.height < button.getBoundingClientRect().top);
  }, [open]);

  const body = html`
    <button
      ref=${buttonRef}
      type="button"
      class="cog-btn"
      data-act=${kind}
      aria-haspopup="menu"
      aria-expanded=${open ? 'true' : 'false'}
      title="More actions"
      aria-label="More actions"
      onClick=${(event) => {
        event.preventDefault();
        event.stopPropagation();
        interactions.toggleMenu(menuKey);
      }}
    ><span class="cog-glyph">⚙︎</span></button>
    <div
      ref=${menuRef}
      class=${`action-menu${open ? ' open' : ''}${opensUp ? ' opens-up' : ''}`}
      data-preact-menu="1"
      role="menu"
    >${children}</div>
  `;
  return wrapperClass ? html`<span class=${wrapperClass}>${body}</span>` : body;
}

export function InlineEditor({
  editorKey, value, type = 'text', className, placeholder, inputProps = {},
  onCommit, children, triggerProps = {},
}) {
  const interactions = useGroupsInteractions();
  const active = interactions.editorKey === editorKey;
  const [draft, setDraft] = useState(String(value ?? ''));
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const inputRef = useRef(null);

  useLayoutEffect(() => {
    if (!active) return;
    setDraft(String(value ?? ''));
    setError('');
    queueMicrotask(() => {
      inputRef.current?.focus();
      inputRef.current?.select?.();
    });
  }, [active, editorKey]);

  if (!active) {
    const { as = 'span', ...props } = triggerProps;
    return h(as, {
      ...props,
      onClick: (event) => {
        event.preventDefault();
        event.stopPropagation();
        interactions.beginEditor(editorKey, event.currentTarget);
      },
      onKeyDown: (event) => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        event.stopPropagation();
        interactions.beginEditor(editorKey, event.currentTarget);
      },
    }, children);
  }

  const cancel = (restoreFocus = false) => {
    if (busyRef.current) return;
    interactions.endEditor(editorKey, restoreFocus);
  };
  const commit = async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError('');
    try {
      await onCommit(draft);
      interactions.endEditor(editorKey);
    } catch (err) {
      setError((err && err.message) || String(err));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };
  return html`<input
    ref=${inputRef}
    type=${type}
    class=${className}
    value=${draft}
    placeholder=${placeholder}
    spellcheck=${false}
    autocomplete="off"
    disabled=${busy}
    ...${inputProps}
    aria-invalid=${error ? 'true' : undefined}
    title=${error || inputProps.title}
    onInput=${(event) => setDraft(event.currentTarget.value)}
    onKeyDown=${(event) => {
      if (event.key === 'Enter') { event.preventDefault(); void commit(); }
      else if (event.key === 'Escape') { event.preventDefault(); cancel(true); }
    }}
    onBlur=${() => cancel()}
  />`;
}
