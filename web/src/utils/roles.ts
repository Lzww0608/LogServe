import type { ConsoleRole, ConsoleSession } from "../types/logserve";

const roleRanks: Record<ConsoleRole, number> = {
  viewer: 1,
  operator: 2,
  admin: 3
};

export function roleAtLeast(session: ConsoleSession | null | undefined, minimum: ConsoleRole): boolean {
  if (!session) return false;
  return (roleRanks[session.role] ?? 0) >= roleRanks[minimum];
}

export function roleLabel(session: ConsoleSession | null | undefined): string {
  return session?.role ?? "signed out";
}
