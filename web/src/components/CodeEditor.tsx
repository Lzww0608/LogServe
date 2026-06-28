import { useId, useMemo, useState, type UIEvent } from "react";
import { copyToClipboard } from "../utils/clipboard";

export function CodeEditor({
  label,
  value,
  onChange,
  disabled = false,
  className = "code-field",
  error,
  language = "python",
  onFormat
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  className?: string;
  error?: string;
  language?: "python" | "json" | "text";
  onFormat?: (value: string) => string;
}) {
  const inputID = useId();
  const labelID = useId();
  const [scroll, setScroll] = useState({ top: 0, left: 0 });
  const lineNumbers = useMemo(() => lineNumberText(value), [value]);
  const highlighted = useMemo(() => highlightCode(value, language), [value, language]);
  const format = () => {
    if (disabled) return;
    onChange(onFormat ? onFormat(value) : normalizeCode(value));
  };
  const handleScroll = (event: UIEvent<HTMLTextAreaElement>) => {
    setScroll({ top: event.currentTarget.scrollTop, left: event.currentTarget.scrollLeft });
  };
  return (
    <div className={`${className} code-editor${error ? " has-error" : ""}`}>
      <div className="code-editor-head">
        <label id={labelID} htmlFor={inputID}>{label}</label>
        <div className="button-row">
          <span className="badge info">{language}</span>
          <button type="button" className="ghost compact-button" onClick={format} disabled={disabled}>Format</button>
          <button type="button" className="ghost compact-button" onClick={() => void copyToClipboard(value)}>Copy</button>
        </div>
      </div>
      <div className={`code-editor-frame${error ? " input-invalid" : ""}`}>
        <pre className="code-line-numbers" aria-hidden="true" style={{ transform: `translateY(${-scroll.top}px)` }}>{lineNumbers}</pre>
        <div className="code-editor-pane">
          <pre
            className="syntax-highlight"
            aria-hidden="true"
            style={{ transform: `translate(${-scroll.left}px, ${-scroll.top}px)` }}
            dangerouslySetInnerHTML={{ __html: highlighted }}
          />
          <textarea
            id={inputID}
            aria-labelledby={labelID}
            value={value}
            onChange={(event) => onChange(event.target.value)}
            onScroll={handleScroll}
            disabled={disabled}
            aria-invalid={Boolean(error)}
            spellCheck={false}
            wrap="off"
            className="code-editor-input"
          />
        </div>
      </div>
      {error && <span className="field-error">{error}</span>}
    </div>
  );
}

function lineNumberText(value: string): string {
  const count = Math.max(1, value.split("\n").length);
  return Array.from({ length: count }, (_, index) => String(index + 1)).join("\n");
}

function normalizeCode(value: string): string {
  return `${value.split("\n").map((line) => line.trimEnd()).join("\n").trimEnd()}\n`;
}

function highlightCode(value: string, language: "python" | "json" | "text"): string {
  const escaped = escapeHTML(value || " ");
  if (language === "json") {
    return escaped.replace(/(&quot;[^&]*?&quot;)(\s*:)?|(\btrue\b|\bfalse\b|\bnull\b)|(-?\b\d+(?:\.\d+)?\b)/g, (match, key, colon, literal, number) => {
      if (key) return `<span class="${colon ? "tok-key" : "tok-string"}">${key}</span>${colon ?? ""}`;
      if (literal) return `<span class="tok-keyword">${literal}</span>`;
      if (number) return `<span class="tok-number">${number}</span>`;
      return match;
    });
  }
  if (language === "python") {
    return escaped
      .replace(/(#.*)$/gm, "<span class=\"tok-comment\">$1</span>")
      .replace(/\b(def|class|return|raise|import|from|if|else|elif|for|while|in|try|except|with|as|None|True|False)\b/g, "<span class=\"tok-keyword\">$1</span>")
      .replace(/(&quot;.*?&quot;|&#39;.*?&#39;)/g, "<span class=\"tok-string\">$1</span>")
      .replace(/\b(\d+(?:\.\d+)?)\b/g, "<span class=\"tok-number\">$1</span>");
  }
  return escaped;
}

function escapeHTML(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}