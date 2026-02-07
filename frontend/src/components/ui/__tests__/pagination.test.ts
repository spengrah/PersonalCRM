import { describe, it, expect } from 'vitest'
import { getPageNumbers } from '../pagination'

describe('getPageNumbers', () => {
  it('returns all pages when total <= 7', () => {
    expect(getPageNumbers(1, 1)).toEqual([1])
    expect(getPageNumbers(1, 3)).toEqual([1, 2, 3])
    expect(getPageNumbers(4, 7)).toEqual([1, 2, 3, 4, 5, 6, 7])
  })

  it('shows trailing ellipsis when current is near start (total=10)', () => {
    expect(getPageNumbers(1, 10)).toEqual([1, 2, 3, 4, 'ellipsis', 10])
    expect(getPageNumbers(2, 10)).toEqual([1, 2, 3, 4, 'ellipsis', 10])
    expect(getPageNumbers(3, 10)).toEqual([1, 2, 3, 4, 'ellipsis', 10])
  })

  it('shows leading ellipsis when current is near end (total=10)', () => {
    expect(getPageNumbers(8, 10)).toEqual([1, 'ellipsis', 7, 8, 9, 10])
    expect(getPageNumbers(9, 10)).toEqual([1, 'ellipsis', 7, 8, 9, 10])
    expect(getPageNumbers(10, 10)).toEqual([1, 'ellipsis', 7, 8, 9, 10])
  })

  it('shows both ellipses when current is in the middle (total=10)', () => {
    expect(getPageNumbers(5, 10)).toEqual([1, 'ellipsis', 4, 5, 6, 'ellipsis', 10])
    expect(getPageNumbers(6, 10)).toEqual([1, 'ellipsis', 5, 6, 7, 'ellipsis', 10])
  })

  it('handles boundary at total=8 (first case with ellipsis)', () => {
    expect(getPageNumbers(1, 8)).toEqual([1, 2, 3, 4, 'ellipsis', 8])
    expect(getPageNumbers(4, 8)).toEqual([1, 'ellipsis', 3, 4, 5, 'ellipsis', 8])
    expect(getPageNumbers(5, 8)).toEqual([1, 'ellipsis', 4, 5, 6, 'ellipsis', 8])
    expect(getPageNumbers(6, 8)).toEqual([1, 'ellipsis', 5, 6, 7, 8])
    expect(getPageNumbers(8, 8)).toEqual([1, 'ellipsis', 5, 6, 7, 8])
  })

  it('never produces duplicate page numbers', () => {
    for (let total = 1; total <= 20; total++) {
      for (let current = 1; current <= total; current++) {
        const result = getPageNumbers(current, total)
        const numbers = result.filter((p): p is number => typeof p === 'number')
        const unique = new Set(numbers)
        expect(unique.size).toBe(numbers.length)
      }
    }
  })

  it('always includes first and last page when total > 7', () => {
    for (let total = 8; total <= 20; total++) {
      for (let current = 1; current <= total; current++) {
        const result = getPageNumbers(current, total)
        const numbers = result.filter((p): p is number => typeof p === 'number')
        expect(numbers[0]).toBe(1)
        expect(numbers[numbers.length - 1]).toBe(total)
      }
    }
  })
})
