'use client'

import type { ComponentType } from 'react'
import { AtSign, Mail, MessageCircle, Phone, Send } from 'lucide-react'
import type { ContactMethodType } from '@/types/contact'

const iconMap: Record<ContactMethodType, ComponentType<{ className?: string }>> = {
  email_personal: Mail,
  email_work: Mail,
  phone: Phone,
  telegram: Send,
  signal: MessageCircle,
  discord: MessageCircle,
  twitter: AtSign,
  gchat: MessageCircle,
  whatsapp: MessageCircle,
}

export function ContactMethodIcon({
  type,
  className = 'h-4 w-4 text-gray-400',
}: {
  type: ContactMethodType
  className?: string
}) {
  const Icon = iconMap[type] ?? AtSign
  return <Icon className={className} />
}
