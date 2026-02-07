import { Button } from './button'

interface PaginationProps {
  page: number
  pages: number
  total: number
  onPageChange: (page: number) => void
  noun?: string
}

function getPageNumbers(current: number, total: number): (number | 'ellipsis')[] {
  if (total <= 7) {
    return Array.from({ length: total }, (_, i) => i + 1)
  }

  const pages: (number | 'ellipsis')[] = [1]

  if (current <= 3) {
    pages.push(2, 3, 4, 'ellipsis', total)
  } else if (current >= total - 2) {
    pages.push('ellipsis', total - 3, total - 2, total - 1, total)
  } else {
    pages.push('ellipsis', current - 1, current, current + 1, 'ellipsis', total)
  }

  return pages
}

export function Pagination({ page, pages, total, onPageChange, noun = 'items' }: PaginationProps) {
  if (pages <= 1) return null

  const pageNumbers = getPageNumbers(page, pages)

  return (
    <nav
      data-testid="pagination"
      aria-label="Pagination"
      className="flex items-center justify-between"
    >
      <div className="text-sm text-gray-700">
        Page {page} of {pages} ({total} total {noun})
      </div>
      <div className="flex items-center space-x-1">
        <Button
          variant="outline"
          size="sm"
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          Previous
        </Button>
        {pageNumbers.map((p, i) =>
          p === 'ellipsis' ? (
            <span key={`ellipsis-${i}`} className="px-2 text-sm text-gray-500" aria-hidden="true">
              &hellip;
            </span>
          ) : (
            <Button
              key={p}
              variant={p === page ? 'primary' : 'outline'}
              size="sm"
              onClick={() => onPageChange(p)}
              className="min-w-[36px]"
              aria-label={`Go to page ${p}`}
              aria-current={p === page ? 'page' : undefined}
            >
              {p}
            </Button>
          )
        )}
        <Button
          variant="outline"
          size="sm"
          disabled={page >= pages}
          onClick={() => onPageChange(page + 1)}
        >
          Next
        </Button>
      </div>
    </nav>
  )
}
