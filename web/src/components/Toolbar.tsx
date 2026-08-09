import { Search } from "lucide-react";

/** A search input, styled consistently across every list page. Filters
 *  client-side over data already fetched — there is no server-side search
 *  endpoint, and there does not need to be for lists this size. */
export function SearchInput({
  value,
  onChange,
  placeholder,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
}) {
  return (
    <div className="relative flex-1">
      <Search size={15} className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-text-muted" />
      <input
        value={value}
        onChange={(e) => { onChange(e.target.value); }}
        placeholder={placeholder}
        className="w-full rounded-lg border border-border bg-bg py-2 pr-3 pl-9 text-sm text-text outline-none placeholder:text-text-muted focus:border-primary"
      />
    </div>
  );
}

/** A filter dropdown, for the "All Status"-style selector every list page
 *  in the reference design carries next to its search box. */
export function FilterSelect<T extends string>({
  value,
  onChange,
  options,
  label,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { value: T; label: string }[];
  /** Accessible name. A row of selects whose first option reads "All states"
   *  is announced as an unlabelled combo box, which tells a screen-reader user
   *  nothing about what it filters. */
  label?: string;
}) {
  return (
    <select
      value={value}
      onChange={(e) => { onChange(e.target.value as T); }}
      {...(label ? { "aria-label": label } : {})}
      className="rounded-lg border border-border bg-bg px-3 py-2 text-sm text-text outline-none focus:border-primary"
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  );
}

export function Toolbar({ children }: { children: React.ReactNode }) {
  return <div className="mb-4 flex flex-wrap items-center gap-3">{children}</div>;
}
