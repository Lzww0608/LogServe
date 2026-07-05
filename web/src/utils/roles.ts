// Frontend role helpers for hiding actions according to the session token role.

import type { ConsoleRole, ConsoleSession } from "../types/logserve";

// roleRanks encodes the viewer < operator < admin permission lattice used by the UI.
const roleRanks: Record<ConsoleRole, number> = {
  viewer: 1,
  operator: 2,
  admin: 3
};

// Compare the current console role against a required minimum role.
// roleAtLeast is intentionally frontend-only; backend role gates remain authoritative.
export function roleAtLeast(session: ConsoleSession | null | undefined, minimum: ConsoleRole): boolean {
  if (!session) return false;
  // Unknown roles degrade to rank 0 so stale sessions cannot unlock controls.
  return (roleRanks[session.role] ?? 0) >= roleRanks[minimum];
}

// Return a compact role label for signed-in and signed-out header states.
export function roleLabel(session: ConsoleSession | null | undefined): string {
  // The signed-out label keeps header rendering simple when no session has loaded.
  return session?.role ?? "signed out";
}
