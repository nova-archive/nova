import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { InfoTerm } from './InfoTerm'
import { GLOSSARY } from './glossary'

afterEach(() => cleanup())

describe('InfoTerm', () => {
  it('renders the term text and a labelled disclosure with the glossary body', () => {
    render(<InfoTerm id="master-key">master key</InfoTerm>)
    // Term text is visible.
    expect(screen.getByText('master key')).toBeInTheDocument()
    // The ⓘ summary exposes the glossary label as its accessible name.
    const summary = screen.getByLabelText(GLOSSARY['master-key'].label)
    expect(summary.tagName.toLowerCase()).toBe('summary')
    // The explanation is present in the DOM (native <details> reveals it).
    expect(screen.getByText(GLOSSARY['master-key'].body)).toBeInTheDocument()
  })

  it('falls back to plain text for an unknown term id', () => {
    render(<InfoTerm id="nope">plain</InfoTerm>)
    expect(screen.getByText('plain')).toBeInTheDocument()
    expect(screen.queryByRole('group')).toBeNull() // no <details>
  })
})
