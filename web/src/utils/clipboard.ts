// Copy text with the browser Clipboard API, returning false when unavailable or denied.

// copyToClipboard hides permission/API failures behind a boolean result for UI feedback.
export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    // Some browsers expose clipboard only in secure contexts or after user gestures.
    if (!navigator.clipboard) return false;
    await navigator.clipboard.writeText(text);
    return true;
  // Clipboard permission denials should not throw into click handlers.
  } catch {
    return false;
  }
}
