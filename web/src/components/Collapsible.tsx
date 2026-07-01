import { ReactNode, useState } from "react";

/**
 * Collapsible is a disclosure section (native <details>) with a styled header.
 * It owns its open/closed state so it survives parent re-renders (e.g. the
 * activity log polls every few seconds). Long lists — activity logs, the egg
 * catalog — default to collapsed so they don't stretch the page.
 */
export function Collapsible({
  title,
  count,
  defaultOpen = false,
  children,
}: {
  title: string;
  count?: number;
  defaultOpen?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <details
      className="collapsible"
      open={open}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary>
        <span>{title}</span>
        {count != null && <span className="collapsible-count">{count}</span>}
      </summary>
      <div className="collapsible-body">{children}</div>
    </details>
  );
}
