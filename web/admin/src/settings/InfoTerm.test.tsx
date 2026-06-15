import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { InfoTerm } from './InfoTerm'

afterEach(() => cleanup())

describe('InfoTerm', () => {
  it('exposes the glossary label as the disclosure accessible name', () => {
    render(<InfoTerm id="paranoid">paranoid</InfoTerm>)
    expect(screen.getByLabelText('What does hardening (paranoid) do?')).toBeInTheDocument()
  })
  it('renders plain text for an unknown id', () => {
    render(<InfoTerm id="nope">plain</InfoTerm>)
    expect(screen.queryByRole('group')).not.toBeInTheDocument()
    expect(screen.getByText('plain')).toBeInTheDocument()
  })
})
