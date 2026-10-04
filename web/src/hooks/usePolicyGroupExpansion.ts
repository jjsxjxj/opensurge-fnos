import { useCallback, useState } from 'react'

const storageKey = 'opensurge.policies.expandedGroups'

// Policy group cards start collapsed, so a long policy list stays scannable.
// Only the groups the operator opened are persisted, which keeps the default for
// every group that has never been touched at "collapsed".
function readExpandedGroups(): Set<string> {
  try {
    const raw = window.localStorage.getItem(storageKey)
    if (!raw) return new Set()
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return new Set()
    return new Set(parsed.filter((value): value is string => typeof value === 'string'))
  } catch {
    return new Set()
  }
}

export function usePolicyGroupExpansion() {
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(readExpandedGroups)

  const toggleExpanded = useCallback((group: string) => {
    setExpandedGroups(previous => {
      const next = new Set(previous)
      if (next.has(group)) next.delete(group)
      else next.add(group)
      try {
        window.localStorage.setItem(storageKey, JSON.stringify([...next]))
      } catch {
        // Storage can be unavailable (for example in a locked-down browser); the
        // in-memory state still carries the choice for this session.
      }
      return next
    })
  }, [])

  return { expandedGroups, toggleExpanded }
}
