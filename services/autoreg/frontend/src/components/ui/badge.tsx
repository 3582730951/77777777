import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const badgeVariants = cva(
  'inline-flex items-center rounded-md border px-2 py-0.5 text-[11px] font-medium',
  {
    variants: {
      variant: {
        default: 'border-[var(--accent-edge)] bg-[var(--accent-soft)] text-[var(--accent)]',
        success: 'border-[var(--badge-success-border)] bg-[var(--badge-success-bg)] text-[var(--badge-success-fg)]',
        warning: 'border-[var(--badge-warning-border)] bg-[var(--badge-warning-bg)] text-[var(--badge-warning-fg)]',
        danger: 'border-[var(--badge-danger-border)] bg-[var(--badge-danger-bg)] text-[var(--badge-danger-fg)]',
        secondary: 'border-[var(--border)] bg-[var(--chip-bg)] text-[var(--text-muted)]',
      },
    },
    defaultVariants: { variant: 'default' },
  }
)

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return <div className={cn(badgeVariants({ variant }), className)} {...props} />
}

export { Badge, badgeVariants }
