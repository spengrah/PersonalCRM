import type { ContactMethod, ContactMethodType } from '@/types/contact'

export const CONTACT_METHOD_TYPE_VALUES: ContactMethodType[] = [
  'email_personal',
  'email_work',
  'phone',
  'telegram',
  'signal',
  'discord',
  'twitter',
  'gchat',
]

export const CONTACT_METHOD_OPTIONS = [
  {
    value: 'email_personal',
    label: 'Personal email',
    placeholder: 'name@example.com',
    inputType: 'email',
  },
  {
    value: 'email_work',
    label: 'Work email',
    placeholder: 'name@company.com',
    inputType: 'email',
  },
  {
    value: 'phone',
    label: 'Phone',
    placeholder: '(555) 555-1234',
    inputType: 'tel',
  },
  {
    value: 'telegram',
    label: 'Telegram',
    placeholder: '@username',
    inputType: 'text',
  },
  {
    value: 'signal',
    label: 'Signal',
    placeholder: 'signal-handle',
    inputType: 'text',
  },
  {
    value: 'discord',
    label: 'Discord',
    placeholder: '@username',
    inputType: 'text',
  },
  {
    value: 'twitter',
    label: 'Twitter',
    placeholder: '@username',
    inputType: 'text',
  },
  {
    value: 'gchat',
    label: 'Google Chat',
    placeholder: 'name@gmail.com',
    inputType: 'email',
  },
]

const HANDLE_METHOD_TYPES = new Set<ContactMethodType>(['telegram', 'discord', 'twitter'])
const EMAIL_METHOD_TYPES = new Set<ContactMethodType>(['email_personal', 'email_work', 'gchat'])

const CONTACT_METHOD_PRIORITY: Record<ContactMethodType, number> = {
  email_personal: 1,
  email_work: 2,
  phone: 3,
  telegram: 4,
  signal: 5,
  discord: 6,
  twitter: 7,
  gchat: 8,
}

export function normalizeContactMethodValue(type: ContactMethodType, value: string) {
  const trimmed = value.trim()
  if (trimmed === '') {
    return ''
  }
  if (HANDLE_METHOD_TYPES.has(type)) {
    return trimmed.replace(/^@+/, '').trim()
  }
  return trimmed
}

export function formatContactMethodValue(type: ContactMethodType, value: string) {
  if (HANDLE_METHOD_TYPES.has(type)) {
    const trimmed = value.trim()
    if (trimmed === '') {
      return ''
    }
    return trimmed.startsWith('@') ? trimmed : `@${trimmed}`
  }
  return value
}

export function getContactMethodLabel(type: ContactMethodType) {
  return CONTACT_METHOD_OPTIONS.find(option => option.value === type)?.label ?? type
}

export function getContactMethodHref(type: ContactMethodType, value: string) {
  if (value.trim() === '') {
    return undefined
  }

  if (type === 'gchat') {
    return undefined
  }

  if (EMAIL_METHOD_TYPES.has(type)) {
    return `mailto:${value}`
  }

  if (type === 'phone') {
    return `tel:${value}`
  }

  if (type === 'telegram') {
    return `https://t.me/${value}`
  }

  if (type === 'twitter') {
    return `https://twitter.com/${value}`
  }

  return undefined
}

export function sortContactMethods(methods: ContactMethod[]) {
  return [...methods].sort((a, b) => {
    if (a.is_primary !== b.is_primary) {
      return a.is_primary ? -1 : 1
    }
    return CONTACT_METHOD_PRIORITY[a.type] - CONTACT_METHOD_PRIORITY[b.type]
  })
}

export function getPrimaryAndSecondaryMethods(
  methods: ContactMethod[] | undefined,
  primaryMethod: ContactMethod | undefined
) {
  if (!methods || methods.length === 0) {
    return { primary: primaryMethod, secondary: undefined }
  }

  const sorted = sortContactMethods(methods)
  const resolvedPrimary = primaryMethod ?? sorted.find(method => method.is_primary) ?? sorted[0]

  const secondary = sorted.find(method => !isSameMethod(method, resolvedPrimary))

  return {
    primary: resolvedPrimary,
    secondary,
  }
}

function isSameMethod(first: ContactMethod | undefined, second: ContactMethod | undefined) {
  if (!first || !second) {
    return false
  }

  if (first.id && second.id) {
    return first.id === second.id
  }

  return first.type === second.type && first.value === second.value
}

export function isEmailMethod(type: ContactMethodType) {
  return EMAIL_METHOD_TYPES.has(type)
}

export function isHandleMethod(type: ContactMethodType) {
  return HANDLE_METHOD_TYPES.has(type)
}
