import { describe, expect, it } from 'vitest'

import { accountHasChatScopes } from '../google-accounts-section'

const CHAT_SCOPE = 'https://www.googleapis.com/auth/chat.spaces.readonly'

describe('accountHasChatScopes', () => {
  it('returns true when the account carries chat.spaces.readonly', () => {
    expect(
      accountHasChatScopes({
        scopes: ['openid', 'email', CHAT_SCOPE],
      })
    ).toBe(true)
  })

  it('returns false when the chat scope is absent (badge hidden, hint shown)', () => {
    expect(
      accountHasChatScopes({
        scopes: ['openid', 'email', 'https://www.googleapis.com/auth/gmail.readonly'],
      })
    ).toBe(false)
  })

  it('returns false when scopes is undefined', () => {
    expect(accountHasChatScopes({ scopes: undefined })).toBe(false)
  })

  it('returns false when scopes is empty', () => {
    expect(accountHasChatScopes({ scopes: [] })).toBe(false)
  })
})
