export function CodeEditor({
  label,
  value,
  onChange,
  disabled = false,
  className = "code-field"
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  className?: string;
}) {
  return (
    <label className={className}>
      {label}
      <textarea value={value} onChange={(event) => onChange(event.target.value)} disabled={disabled} />
    </label>
  );
}
