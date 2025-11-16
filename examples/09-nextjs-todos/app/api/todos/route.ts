import { NextRequest, NextResponse } from "next/server";
import { getAllTodos, createTodo, deleteAllTodos } from "@/lib/db";

// GET /api/todos - Get all todos
export async function GET() {
  try {
    const todos = getAllTodos();
    return NextResponse.json(todos);
  } catch (error) {
    console.error("Error fetching todos:", error);
    return NextResponse.json(
      { error: "Failed to fetch todos" },
      { status: 500 }
    );
  }
}

// POST /api/todos - Create a new todo
export async function POST(request: NextRequest) {
  try {
    const body = await request.json();

    if (!body.title || typeof body.title !== "string") {
      return NextResponse.json(
        { error: "Title is required and must be a string" },
        { status: 400 }
      );
    }

    const todo = createTodo({ title: body.title.trim() });
    return NextResponse.json(todo, { status: 201 });
  } catch (error) {
    console.error("Error creating todo:", error);
    return NextResponse.json(
      { error: "Failed to create todo" },
      { status: 500 }
    );
  }
}

// DELETE /api/todos - Delete all todos
export async function DELETE() {
  try {
    const count = deleteAllTodos();
    return NextResponse.json({ deleted: count });
  } catch (error) {
    console.error("Error deleting todos:", error);
    return NextResponse.json(
      { error: "Failed to delete todos" },
      { status: 500 }
    );
  }
}
