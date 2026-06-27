export function statusTone(value: string) {
  if (["SUCCEEDED", "COMPLETED", "ACTIVE"].includes(value)) return "good";
  if (["FAILED", "UNAVAILABLE"].includes(value)) return "bad";
  if (["RUNNING", "STARTED"].includes(value)) return "info";
  if (["QUEUED", "SCHEDULED"].includes(value)) return "warn";
  return "neutral";
}

export function errorMessage(error: unknown) {
  if (error instanceof Error) {
    const code = "code" in error && typeof error.code === "string" ? error.code : "";
    return code ? `${code}: ${error.message}` : error.message;
  }
  return String(error);
}
