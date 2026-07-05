// Map backend lifecycle states into the console badge tone vocabulary.

// statusTone groups task, workflow, actor, and worker states into shared badge colors.
export function statusTone(value: string) {
  // Different backend domains share badge tones through normalized status strings.
  if (["SUCCEEDED", "COMPLETED", "ACTIVE"].includes(value)) return "good";
  if (["FAILED", "UNAVAILABLE"].includes(value)) return "bad";
  if (["RUNNING", "STARTED"].includes(value)) return "info";
  if (["QUEUED", "SCHEDULED"].includes(value)) return "warn";
  return "neutral";
}

// Normalize thrown values into the short error text shown in panels and forms.
export function errorMessage(error: unknown) {
  if (error instanceof Error) {
    // APIError carries code as an own property; generic Error values fall back to message only.
    const code = "code" in error && typeof error.code === "string" ? error.code : "";
    return code ? `${code}: ${error.message}` : error.message;
  }
  // Non-Error throws are rare but can come from rejected promises or JSON parsing helpers.
  return String(error);
}
