export interface SystemTimeResponse {
  current_time: string
  is_accelerated: boolean
  acceleration_factor: number
  environment: string
  base_time: string
}

export interface AccelerationSettings {
  enabled: boolean
  acceleration_factor: number
  base_time: string
}

export interface SetAccelerationRequest {
  enabled: boolean
  acceleration_factor: number
  base_time: string
}
