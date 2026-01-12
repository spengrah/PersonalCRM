import { describe, it, expect } from 'vitest'
import { inferEmailType, isFreeEmailDomain } from '../email-type-inference'

describe('email-type-inference', () => {
  describe('isFreeEmailDomain', () => {
    it('returns true for common free email domains', () => {
      expect(isFreeEmailDomain('gmail.com')).toBe(true)
      expect(isFreeEmailDomain('yahoo.com')).toBe(true)
      expect(isFreeEmailDomain('hotmail.com')).toBe(true)
      expect(isFreeEmailDomain('outlook.com')).toBe(true)
      expect(isFreeEmailDomain('icloud.com')).toBe(true)
      expect(isFreeEmailDomain('protonmail.com')).toBe(true)
    })

    it('returns false for corporate domains', () => {
      expect(isFreeEmailDomain('company.com')).toBe(false)
      expect(isFreeEmailDomain('acme.co')).toBe(false)
      expect(isFreeEmailDomain('enterprise.org')).toBe(false)
    })

    it('handles case-insensitive matching', () => {
      expect(isFreeEmailDomain('Gmail.com')).toBe(true)
      expect(isFreeEmailDomain('GMAIL.COM')).toBe(true)
      expect(isFreeEmailDomain('GmAiL.CoM')).toBe(true)
    })

    it('returns false for empty or invalid input', () => {
      expect(isFreeEmailDomain('')).toBe(false)
    })
  })

  describe('inferEmailType', () => {
    describe('with free email domains', () => {
      it('returns email_personal for gmail addresses', () => {
        expect(inferEmailType('john@gmail.com')).toBe('email_personal')
      })

      it('returns email_personal for yahoo addresses', () => {
        expect(inferEmailType('jane@yahoo.com')).toBe('email_personal')
      })

      it('returns email_personal for hotmail addresses', () => {
        expect(inferEmailType('test@hotmail.com')).toBe('email_personal')
      })

      it('returns email_personal for outlook addresses', () => {
        expect(inferEmailType('user@outlook.com')).toBe('email_personal')
      })

      it('returns email_personal for icloud addresses', () => {
        expect(inferEmailType('apple@icloud.com')).toBe('email_personal')
      })
    })

    describe('with corporate domains', () => {
      it('returns email_work for company domains', () => {
        expect(inferEmailType('john@company.com')).toBe('email_work')
      })

      it('returns email_work for enterprise domains', () => {
        expect(inferEmailType('jane@enterprise.org')).toBe('email_work')
      })

      it('returns email_work for startup domains', () => {
        expect(inferEmailType('developer@startup.io')).toBe('email_work')
      })
    })

    describe('with original type hints', () => {
      it('respects work hint for free email domains', () => {
        // If original type hint says work, use it even for gmail
        expect(inferEmailType('john@gmail.com', 'work')).toBe('email_work')
      })

      it('respects personal hint for corporate domains', () => {
        // If original type hint says personal, use it even for company domain
        expect(inferEmailType('john@company.com', 'personal')).toBe('email_personal')
      })

      it('respects home hint as personal', () => {
        expect(inferEmailType('john@company.com', 'home')).toBe('email_personal')
      })

      it('treats "other" hint as work email', () => {
        // The implementation treats 'other' as a work type hint
        expect(inferEmailType('john@gmail.com', 'other')).toBe('email_work')
        expect(inferEmailType('john@company.com', 'other')).toBe('email_work')
      })

      it('falls back to domain inference for unknown hints', () => {
        expect(inferEmailType('john@gmail.com', 'unknown')).toBe('email_personal')
        expect(inferEmailType('john@company.com', 'unknown')).toBe('email_work')
      })
    })

    describe('edge cases', () => {
      it('handles uppercase email addresses', () => {
        expect(inferEmailType('JOHN@GMAIL.COM')).toBe('email_personal')
        expect(inferEmailType('JANE@COMPANY.COM')).toBe('email_work')
      })

      it('handles mixed case email addresses', () => {
        expect(inferEmailType('John.Doe@Gmail.Com')).toBe('email_personal')
      })

      it('returns email_personal for invalid email (no @)', () => {
        expect(inferEmailType('notanemail')).toBe('email_personal')
      })

      it('returns email_personal for empty string', () => {
        expect(inferEmailType('')).toBe('email_personal')
      })

      it('handles emails with subdomains', () => {
        expect(inferEmailType('user@mail.company.com')).toBe('email_work')
      })
    })
  })
})
