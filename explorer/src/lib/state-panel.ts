export type PanelState =
  | 'no_events'
  | 'not_indexed'
  | 'invalid_contract'
  | 'api_unreachable'
  | 'not_found'
  | 'info';

export interface PanelAction {
  label: string;
  href: string;
  variant?: 'primary' | 'ghost';
  external?: boolean;
}