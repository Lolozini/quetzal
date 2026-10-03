import { Component, ReactNode } from "react";

// ErrorBoundary keeps a page that fails to render from taking the whole panel
// down with it. React unmounts the tree on an error it does not catch, which
// left a blank page with nothing to click: one template without variables did
// that to the create form, for every account.
export class ErrorBoundary extends Component<
  { fallback: (error: Error) => ReactNode; children: ReactNode },
  { error: Error | null }
> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    return this.state.error ? this.props.fallback(this.state.error) : this.props.children;
  }
}
