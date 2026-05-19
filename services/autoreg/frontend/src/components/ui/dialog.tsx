import { useEffect, useRef } from 'react'
import type { CSSProperties, KeyboardEvent, ReactNode } from 'react'
import { cn } from '@/lib/utils'

const FOCUSABLE_SELECTOR = [
  'a[href]',
  'area[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  'iframe',
  'object',
  'embed',
  '[contenteditable="true"]',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function getFocusable(container: HTMLElement | null) {
  if (!container) return []
  return Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR))
    .filter(el => !el.hasAttribute('disabled') && !el.getAttribute('aria-hidden') && (
      el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0
    ))
}

type DialogProps = {
  titleId: string
  children: ReactNode
  onClose: () => void
  className?: string
  style?: CSSProperties
  ariaDescribedBy?: string
  closeOnBackdrop?: boolean
}

export function Dialog({
  titleId,
  children,
  onClose,
  className,
  style,
  ariaDescribedBy,
  closeOnBackdrop = true,
}: DialogProps) {
  const panelRef = useRef<HTMLDivElement>(null)
  const restoreFocusRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    if (typeof document === 'undefined') return

    restoreFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const frame = window.requestAnimationFrame(() => {
      const panel = panelRef.current
      const firstFocusable = getFocusable(panel)[0]
      ;(firstFocusable || panel)?.focus()
    })

    return () => {
      window.cancelAnimationFrame(frame)
      restoreFocusRef.current?.focus?.()
    }
  }, [])

  const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape') {
      event.stopPropagation()
      onClose()
      return
    }

    if (event.key !== 'Tab') return

    const focusable = getFocusable(panelRef.current)
    if (focusable.length === 0) {
      event.preventDefault()
      panelRef.current?.focus()
      return
    }

    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    const active = document.activeElement

    if (event.shiftKey && active === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && active === last) {
      event.preventDefault()
      first.focus()
    }
  }

  return (
    <div
      className="dialog-backdrop"
      onClick={event => {
        if (closeOnBackdrop && event.target === event.currentTarget) onClose()
      }}
    >
      <div
        ref={panelRef}
        className={cn('dialog-panel', className)}
        style={style}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={ariaDescribedBy}
        tabIndex={-1}
        onKeyDown={handleKeyDown}
      >
        {children}
      </div>
    </div>
  )
}
