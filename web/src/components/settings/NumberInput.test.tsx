import { useEffect, useState } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { NumberInput } from './NumberInput';

const props = {
  label: 'Escalation max wait (minutes)',
  min: 1,
  max: 240,
  placeholder: '10',
};

function DeferredNumberInput({ onCommit }: { onCommit: (value: number) => void }) {
  const [visible, setVisible] = useState(false);
  useEffect(() => {
    void Promise.resolve().then(() => setVisible(true));
  }, []);
  return visible ? <NumberInput {...props} value={10} onCommit={onCommit} /> : null;
}

function observeInput(): Promise<HTMLInputElement> {
  return new Promise((resolve) => {
    const find = () => {
      const input = document.querySelector<HTMLInputElement>('input[aria-label="Escalation max wait (minutes)"]');
      if (!input) return false;
      observer.disconnect();
      resolve(input);
      return true;
    };
    const observer = new MutationObserver(find);
    observer.observe(document.body, { childList: true, subtree: true });
    queueMicrotask(find);
  });
}

describe('NumberInput', () => {
  it('keeps a value typed immediately after the input appears (mount-time sync must not revert it)', async () => {
    const onCommit = vi.fn();
    const actEnvironment = globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean };
    const previousActEnvironment = actEnvironment.IS_REACT_ACT_ENVIRONMENT;
    actEnvironment.IS_REACT_ACT_ENVIRONMENT = false;
    try {
      render(<DeferredNumberInput onCommit={onCommit} />);
      const input = await observeInput();

      fireEvent.change(input, { target: { value: '25' } });
      expect(input).toHaveValue(25);
      fireEvent.blur(input);
      expect(onCommit).toHaveBeenCalledTimes(1);
      expect(onCommit).toHaveBeenCalledWith(25);
    } finally {
      actEnvironment.IS_REACT_ACT_ENVIRONMENT = previousActEnvironment;
    }
  });

  it('commits Enter and clamps to max', () => {
    const onCommit = vi.fn();
    render(<NumberInput {...props} value={10} onCommit={onCommit} />);
    const input = screen.getByRole('spinbutton', { name: props.label });
    fireEvent.change(input, { target: { value: '999' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(input).toHaveValue(240);
    expect(onCommit).toHaveBeenCalledWith(240);
  });

  it('Escape restores the prop value', () => {
    const onCommit = vi.fn();
    render(<NumberInput {...props} value={10} onCommit={onCommit} />);
    const input = screen.getByRole('spinbutton', { name: props.label });
    fireEvent.change(input, { target: { value: '25' } });
    fireEvent.keyDown(input, { key: 'Escape' });
    expect(input).toHaveValue(10);
    expect(onCommit).not.toHaveBeenCalled();
  });

  it('a prop change resets the draft', () => {
    const onCommit = vi.fn();
    const view = render(<NumberInput {...props} value={10} onCommit={onCommit} />);
    const input = screen.getByRole('spinbutton', { name: props.label });
    fireEvent.change(input, { target: { value: '25' } });
    view.rerender(<NumberInput {...props} value={15} onCommit={onCommit} />);
    expect(input).toHaveValue(15);
    expect(onCommit).not.toHaveBeenCalled();
  });

  it('rounds to a fractional step on commit', () => {
    const onCommit = vi.fn();
    render(<NumberInput label="Sidebar text size" value={1} min={0.7} max={1.4} step={0.05} onCommit={onCommit} />);
    const input = screen.getByRole('spinbutton', { name: 'Sidebar text size' });
    fireEvent.change(input, { target: { value: '0.93' } });
    fireEvent.blur(input);
    expect(onCommit).toHaveBeenCalledWith(0.95);
    expect(input).toHaveValue(0.95);
  });

  it('live mode applies a valid typed value without waiting for blur, and skips intermediate ones', () => {
    function Live() {
      const [value, setValue] = useState(13);
      return <>
        <NumberInput label="Font size" value={value} min={10} max={22} live onCommit={setValue} />
        <output data-testid="applied">{value}</output>
      </>;
    }
    render(<Live />);
    const input = screen.getByRole('spinbutton', { name: 'Font size' });
    const applied = screen.getByTestId('applied');
    // Typing "18" over "13": "1" is out of range, so nothing applies and the
    // box keeps the "1" instead of jumping to the 10 px floor.
    fireEvent.change(input, { target: { value: '1' } });
    expect(input).toHaveValue(1);
    expect(applied).toHaveTextContent('13');
    fireEvent.change(input, { target: { value: '18' } });
    expect(input).toHaveValue(18);
    expect(applied).toHaveTextContent('18');
    // An empty box is allowed while typing and restores on blur.
    fireEvent.change(input, { target: { value: '' } });
    expect(input).toHaveValue(null);
    fireEvent.blur(input);
    expect(input).toHaveValue(18);
    expect(applied).toHaveTextContent('18');
  });

  it('live mode keeps a fractional draft while it applies the rounded value', () => {
    function Live() {
      const [value, setValue] = useState(1);
      return <>
        <NumberInput label="Sidebar text size" value={value} min={0.7} max={1.4} step={0.05} live onCommit={setValue} />
        <output data-testid="applied">{value}</output>
      </>;
    }
    render(<Live />);
    const input = screen.getByRole('spinbutton', { name: 'Sidebar text size' });
    const applied = screen.getByTestId('applied');
    fireEvent.change(input, { target: { value: '0' } });
    expect(applied).toHaveTextContent('1');
    fireEvent.change(input, { target: { value: '0.9' } });
    expect(input).toHaveValue(0.9);
    expect(applied).toHaveTextContent('0.9');
    fireEvent.change(input, { target: { value: '1.22' } });
    expect(input).toHaveValue(1.22);
    expect(applied).toHaveTextContent('1.2');
    fireEvent.blur(input);
    expect(input).toHaveValue(1.2);
  });

  it('without live, typing never commits before blur', () => {
    const onCommit = vi.fn();
    render(<NumberInput {...props} value={10} onCommit={onCommit} />);
    const input = screen.getByRole('spinbutton', { name: props.label });
    fireEvent.change(input, { target: { value: '25' } });
    expect(onCommit).not.toHaveBeenCalled();
  });
});
