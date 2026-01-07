# Google Calendar Sync

**Last Updated**: January 2026

This document describes the Google Calendar sync feature that allows PersonalCRM to sync calendar events from Google Calendar, update `last_contacted` timestamps automatically after meetings, and display upcoming meetings on contact detail pages.

---

## Table of Contents

1. [Overview](#overview)
2. [Features](#features)
3. [Architecture](#architecture)
4. [API Endpoints](#api-endpoints)
5. [Database Schema](#database-schema)
6. [Configuration](#configuration)
7. [How It Works](#how-it-works)
8. [Frontend Integration](#frontend-integration)

---

## Overview

The Google Calendar sync feature integrates with Google Calendar to:

- **Sync calendar events** from the user's primary Google Calendar
- **Match attendees** to CRM contacts automatically using email addresses
- **Update `last_contacted`** timestamps after meetings occur
- **Display upcoming meetings** on contact detail pages

This feature requires Google OAuth2 authentication and the `https://www.googleapis.com/auth/calendar.readonly` scope.

---

## Features

### Event Syncing

- Syncs events from the past 30 days to 30 days in the future
- Uses incremental sync tokens for efficient updates after initial sync
- Skips all-day events (holidays, birthdays) that aren't meetings
- Skips declined events

### Attendee Matching

- Extracts attendee email addresses from calendar events
- Matches emails to CRM contacts using the identity service
- Creates unmatched identity records for potential future matching

### Last Contacted Updates

- Automatically updates `last_contacted` for matched contacts after events end
- Runs as part of the sync process
- Only updates for confirmed (not cancelled or declined) events

### Upcoming Meetings Display

- Shows upcoming meetings on the contact detail page
- Displays meeting title, time, location, and attendee count
- Hides automatically when no upcoming meetings exist

---

## Architecture

### Components

| Component | File | Description |
|-----------|------|-------------|
| Migration | `backend/migrations/015_calendar_event.up.sql` | Database schema for calendar events |
| SQLC Queries | `backend/internal/db/queries/calendar_event.sql` | SQL queries for event operations |
| Repository | `backend/internal/repository/calendar.go` | Data access layer for calendar events |
| Sync Provider | `backend/internal/google/calendar.go` | Google Calendar sync implementation |
| HTTP Handler | `backend/internal/api/handlers/calendar.go` | REST API endpoints |
| Frontend Hook | `frontend/src/hooks/use-calendar.ts` | React Query hooks for calendar data |
| Frontend Component | `frontend/src/components/contacts/upcoming-meetings.tsx` | Upcoming meetings UI |

### Sync Flow

1. **Initial Sync**: Fetches events ±30 days from now, stores a sync token
2. **Incremental Sync**: Uses sync token to fetch only changed events
3. **Event Processing**: For each event:
   - Skip all-day events and declined events
   - Parse start/end times
   - Build attendee list
   - Match attendees to CRM contacts
   - Upsert event to database
4. **Last Contacted Update**: After sync, find past events that need `last_contacted` updates

---

## API Endpoints

### List Events for Contact

```
GET /api/v1/contacts/:id/events
```

Query parameters:
- `limit` (optional, default 20)
- `offset` (optional, default 0)

Returns all calendar events involving a specific contact, ordered by start time descending.

### List Upcoming Events for Contact

```
GET /api/v1/contacts/:id/events/upcoming
```

Query parameters:
- `limit` (optional, default 10)

Returns upcoming calendar events for a specific contact, ordered by start time ascending.

### List All Upcoming Events

```
GET /api/v1/events/upcoming
```

Query parameters:
- `limit` (optional, default 20)
- `offset` (optional, default 0)

Returns all upcoming events that have matched CRM contacts.

---

## Database Schema

### calendar_event table

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID | Primary key |
| `gcal_event_id` | TEXT | Google Calendar event ID |
| `gcal_calendar_id` | TEXT | Google Calendar ID (default: "primary") |
| `google_account_id` | TEXT | Google account email |
| `title` | TEXT | Event title/summary |
| `description` | TEXT | Event description |
| `location` | TEXT | Event location |
| `start_time` | TIMESTAMPTZ | Event start time |
| `end_time` | TIMESTAMPTZ | Event end time |
| `all_day` | BOOLEAN | Is all-day event |
| `status` | TEXT | Event status (confirmed/tentative/cancelled) |
| `user_response` | TEXT | User's response (accepted/declined/tentative/needsAction) |
| `organizer_email` | TEXT | Organizer's email |
| `attendees` | JSONB | List of attendees with email, name, response |
| `matched_contact_ids` | UUID[] | Array of matched CRM contact IDs |
| `synced_at` | TIMESTAMPTZ | Last sync timestamp |
| `last_contacted_updated` | BOOLEAN | Whether last_contacted was updated for this event |

### Indexes

- `idx_calendar_event_contacts`: GIN index on `matched_contact_ids` for contact lookups
- `idx_calendar_event_start`: Partial index on `start_time` for upcoming events
- `idx_calendar_event_end`: Partial index on `end_time` for past events
- `idx_calendar_event_account`: Index on `google_account_id` for multi-account support
- `idx_calendar_event_needs_update`: Partial index for events needing `last_contacted` updates

---

## Configuration

The calendar sync feature is enabled when:

1. `ENABLE_EXTERNAL_SYNC=true` is set
2. Google OAuth is configured with `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET`
3. The user has connected a Google account with calendar scope

### Sync Interval

Default sync interval: **15 minutes**

This can be adjusted by modifying `CalendarDefaultInterval` in `backend/internal/google/calendar.go`.

---

## How It Works

### Event Matching

When an event is processed, the sync provider:

1. Extracts all attendee emails (excluding the user)
2. For each email, calls `identityService.MatchOrCreate()` in discovery mode
3. Collects all matched contact IDs
4. Stores matched contact IDs in the `matched_contact_ids` array

### Last Contacted Updates

After sync completes:

1. Query events where `end_time < now` AND `last_contacted_updated = FALSE`
2. For each event, update `last_contacted` for all matched contacts to `end_time`
3. Mark event as `last_contacted_updated = TRUE`

This ensures contacts are only updated once per event, and the update uses the actual event end time.

---

## Frontend Integration

### Using the Hook

```tsx
import { useUpcomingEventsForContact } from '@/hooks/use-calendar'

function MyComponent({ contactId }: { contactId: string }) {
  const { data: events, isLoading } = useUpcomingEventsForContact(contactId, 5)

  if (isLoading) return <LoadingSpinner />
  if (!events?.length) return null

  return (
    <ul>
      {events.map(event => (
        <li key={event.id}>{event.title}</li>
      ))}
    </ul>
  )
}
```

### UpcomingMeetings Component

The `UpcomingMeetings` component is designed to be dropped into any contact page:

```tsx
import { UpcomingMeetings } from '@/components/contacts/upcoming-meetings'

<UpcomingMeetings contactId={contactId} />
```

The component:
- Shows a loading skeleton while fetching
- Hides automatically if no upcoming meetings exist
- Displays up to 5 upcoming meetings by default
- Shows meeting title, time, location, and attendee count
