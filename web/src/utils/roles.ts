// Frontend role helpers for hiding actions according to the session token role.

import type { ConsoleRole, ConsoleSession } from "../types/logserve";

const roleRanks: Record<ConsoleRole, number> = {
  viewer: 1,
  operator: 2,
  admin: 3
};

// Compare the current console role against a required minimum role.
export function roleAtLeast(session: ConsoleSession | null | undefined, minimum: ConsoleRole): boolean {
  if (!session) return false;
  return (roleRanks[session.role] ?? 0) >= roleRanks[minimum];
}

// Return a compact role label for signed-in and signed-out header states.
export function roleLabel(session: ConsoleSession | null | undefined): string {
  return session?.role ?? "signed out";
}
