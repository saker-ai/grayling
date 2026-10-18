import type { ButtonHTMLAttributes, ReactElement, ReactNode, Ref } from 'react';

// Button maps to the legacy .btn classes: variant → primary/danger, size sm,
// grow for full width. Native type defaults to "button" so it never submits
// a wrapping form by accident (legacy set type=button explicitly). React 19
// passes ref as a plain prop; it is forwarded to the underlying <button> so
// callers (e.g. Pagination) can manage focus.
export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'default' | 'primary' | 'danger';
  size?: 'default' | 'sm';
  grow?: boolean;
  children?: ReactNode;
  ref?: Ref<HTMLButtonElement>;
}

export function Button({
  variant = 'default',
  size = 'default',
  grow = false,
  className = '',
  type = 'button',
  children,
  ref,
  ...rest
}: ButtonProps): ReactElement {
  const classes = ['btn'];
  if (variant !== 'default') classes.push(variant);
  if (size === 'sm') classes.push('sm');
  if (grow) classes.push('grow');
  if (className) classes.push(className);
  return (
    <button type={type} className={classes.join(' ')} ref={ref} {...rest}>
      {children}
    </button>
  );
}
