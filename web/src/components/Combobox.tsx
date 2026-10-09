import { useEffect, useId, useRef, useState } from "react";

export interface ComboOption {
  value: string;
  label: string;
}

/**
 * Combobox is a single control that looks like a dropdown but filters as you
 * type: click to open (shows every option), start typing to narrow by label,
 * click or Enter to pick. No separate search field — the input is the picker.
 */
export function Combobox({
  id,
  options,
  value,
  onChange,
  placeholder,
  emptyLabel = "No matches.",
}: {
  id?: string;
  options: ComboOption[];
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  emptyLabel?: string;
}) {
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const [active, setActive] = useState(0);
  const ref = useRef<HTMLDivElement>(null);
  const listId = useId();

  const selected = options.find((o) => o.value === value);
  const q = filter.trim().toLowerCase();
  const visible = q ? options.filter((o) => o.label.toLowerCase().includes(q)) : options;
  const activeIndex = Math.max(0, Math.min(active, visible.length - 1));
  const activeId = open && visible.length > 0 ? `${listId}-option-${activeIndex}` : undefined;

  useEffect(() => {
    if (activeId) document.getElementById(activeId)?.scrollIntoView({ block: "nearest" });
  }, [activeId]);

  // Close when clicking outside the control.
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  function openList() {
    setOpen(true);
    setFilter("");
    setActive(Math.max(0, options.findIndex((o) => o.value === value)));
  }

  function choose(v: string) {
    onChange(v);
    setOpen(false);
    setFilter("");
  }

  return (
    <div className="combobox" ref={ref} onBlur={(e) => {
      if (!e.currentTarget.contains(e.relatedTarget)) {
        setOpen(false);
        setFilter("");
      }
    }}>
      <input id={id}
        type="text"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-activedescendant={activeId}
        value={open ? filter : selected?.label ?? ""}
        placeholder={placeholder}
        onFocus={openList}
        onMouseDown={() => {
          if (!open) openList();
        }}
        onChange={(e) => {
          setFilter(e.target.value);
          setOpen(true);
          setActive(0);
        }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setOpen(true);
            setActive(open ? Math.min(activeIndex + 1, Math.max(0, visible.length - 1)) : 0);
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setOpen(true);
            setActive(open ? Math.max(activeIndex - 1, 0) : Math.max(0, visible.length - 1));
          } else if (e.key === "Enter") {
            if (open) {
              e.preventDefault();
              if (visible[activeIndex]) choose(visible[activeIndex].value);
            }
          } else if (e.key === "Escape") {
            if (open) e.preventDefault();
            setOpen(false);
          }
        }}
      />
      <span className="combobox-caret">▾</span>
      {open && (
        <div className="combobox-list" id={listId} role="listbox" aria-label={placeholder}>
          {visible.map((o, i) => (
            <div
              key={o.value}
              id={`${listId}-option-${i}`}
              role="option"
              aria-selected={o.value === value}
              className={"combobox-item" + (i === activeIndex ? " active" : "")}
              onMouseDown={(e) => {
                e.preventDefault();
                choose(o.value);
              }}
              onMouseEnter={() => setActive(i)}
            >
              {o.label}
            </div>
          ))}
          {visible.length === 0 && <div className="combobox-empty" role="option" aria-disabled="true">{emptyLabel}</div>}
        </div>
      )}
    </div>
  );
}
