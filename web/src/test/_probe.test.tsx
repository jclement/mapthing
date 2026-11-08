import { render } from "@testing-library/react";
import { Field, Input } from "../components/ui";
it("probe", () => {
  const { container } = render(<div><Field label="A">{(id) => <Input id={id} />}</Field><Field label="B">{(id) => <Input id={id} type="number" />}</Field></div>);
  console.log(container.innerHTML);
});
