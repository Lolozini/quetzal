import { useEffect, useRef, useState } from "react";

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
  options,
  value,
  onChange,
  placeholder,
  emptyLabel = "No matches.",
}: {
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

  const selected = options.find((o) => o.value === value);
  const q = filter.trim().toLowerCase();
  const visible = q ? options.filter((o) => o.label.toLowerCase().includes(q)) : options;

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
    setActive(0);
  }

  function choose(v: string) {
    onChange(v);
    setOpen(false);
    setFilter("");
  }

  return (
    <div className="combobox" ref={ref}>
      <input
        type="text"
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
            setActive((a) => Math.min(a + 1, visible.length - 1));
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setActive((a) => Math.max(a - 1, 0));
          } else if (e.key === "Enter") {
            if (open && visible[active]) {
              e.preventDefault();
              choose(visible[active].value);
            }
          } else if (e.key === "Escape") {
            setOpen(false);
          }
        }}
      />
      <span className="combobox-caret">▾</span>
      {open && (
        <div className="combobox-list">
          {visible.map((o, i) => (
            <div
              key={o.value}
              className={"combobox-item" + (i === active ? " active" : "")}
              onMouseDown={(e) => {
                e.preventDefault();
                choose(o.value);
              }}
              onMouseEnter={() => setActive(i)}
            >
              {o.label}
            </div>
          ))}
          {visible.length === 0 && <div className="combobox-empty">{emptyLabel}</div>}
        </div>
      )}
    </div>
  );
}
