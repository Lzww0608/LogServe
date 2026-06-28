export function CodeEditor({
  label,
  value,
  onChange,
  disabled = false,
  className = "code-field",
  error
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  className?: string;
  error?: string;
}) {
  return (
    <label className={className}>
      {label}
      <textarea
        value={value}
        onChange={(event) => onChange(event.target.value)}
        disabled={disabled}
        aria-invalid={Boolean(error)}
        className={error ? "input-invalid" : undefined}
      />
      {error && <span className="field-error">{error}</span>}
    </label>
  );
}
