import { Flag, ListChecks, Quote, type LucideIcon } from 'lucide-react'
import type { FlagKind } from '@/types/generated/judgment'

/**
 * How each flag kind shows: its chip name, its icon and the icon's color. The
 * record is keyed by the generated FlagKind, so a kind added to it without an
 * entry here does not compile; the "+ Flag" menu lists these entries.
 */
export const FLAG_KINDS: Record<FlagKind, { name: string; icon: LucideIcon; iconClass: string }> = {
  commitment: { name: 'Commitment', icon: ListChecks, iconClass: 'text-blue-700' },
  big_moment: { name: 'Big moment', icon: Flag, iconClass: 'text-pink-800' },
  claim: { name: 'Claim', icon: Quote, iconClass: 'text-teal-700' },
}

export const FLAG_KIND_ORDER = Object.keys(FLAG_KINDS) as FlagKind[]
