import { describe, expect, it } from 'vitest'

import { accountHasChatScopes } from '../google-accounts-section'

const SPACES_SCOPE = 'https://www.googleapis.com/auth/chat.spaces.readonly'
const MESSAGES_SCOPE = 'https://www.googleapis.com/auth/chat.messages.readonly'
const MEMBERSHIPS_SCOPE = 'https://www.googleapis.com/auth/chat.memberships.readonly'
const ALL_CHAT_SCOPES = [SPACES_SCOPE, MESSAGES_SCOPE, MEMBERSHIPS_SCOPE]

describe('accountHasChatScopes', () => {
  it('returns true when the account carries ALL THREE chat scopes', () => {
    expect(
      accountHasChatScopes({
        scopes: ['openid', 'email', ...ALL_CHAT_SCOPES],
      })
    ).toBe(true)
  })

  it('returns false for a partial grant — only spaces.readonly', () => {
    expect(accountHasChatScopes({ scopes: ['openid', 'email', SPACES_SCOPE] })).toBe(false)
  })

  it('returns false for a partial grant — only messages.readonly', () => {
    expect(accountHasChatScopes({ scopes: ['openid', 'email', MESSAGES_SCOPE] })).toBe(false)
  })

  it('returns false for a partial grant — missing memberships.readonly', () => {
    expect(accountHasChatScopes({ scopes: ['openid', SPACES_SCOPE, MESSAGES_SCOPE] })).toBe(false)
  })

  it('returns false when no chat scopes are present (badge hidden, hint shown)', () => {
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
